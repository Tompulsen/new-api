package jsplugin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

type videoReferenceError struct {
	message string
	status  int
}

func (e *videoReferenceError) Error() string       { return e.message }
func (e *videoReferenceError) HTTPStatusCode() int { return e.status }

type videoReferenceUploader struct {
	adaptor *TaskAdaptor
	info    *relaycommon.RelayInfo
	urls    map[string]string
}

var generationFileID = regexp.MustCompile(`^file_[A-Za-z0-9_-]{16,100}$`)

func (u *videoReferenceUploader) discard() {
	if u == nil || u.adaptor.plugin.Meta.Key != "sd-compatible" {
		return
	}
	for _, link := range u.urls {
		if err := service.RemoveVideoReference(link); err != nil {
			common.SysError("could not remove an unused video reference image")
		}
	}
	clear(u.urls)
}

func (u *videoReferenceUploader) resolve(c *gin.Context, placeholder map[string]any, form *multipart.Form, limit int64, total *int64) (string, error) {
	reject := func(message string) (string, error) {
		return "", &videoReferenceError{message: message, status: http.StatusBadRequest}
	}
	async := u.adaptor.plugin.Meta.Key == "async-compatible"
	sd := u.adaptor.plugin.Meta.Key == "sd-compatible"
	if !u.info.HasChannelMeta() || (!async && !sd && (u.adaptor.plugin.Meta.Key != "sora" || u.info.ChannelSetting.VideoRequestFormat != "json")) {
		return reject("video reference uploads require the channel JSON video format")
	}
	source, ok := placeholder["__videoUpload"].(map[string]any)
	if len(placeholder) != 1 || !ok {
		return reject("invalid video reference upload")
	}
	mediaType := "image"
	if async {
		if value, exists := source["mediaType"]; exists {
			mediaType, _ = value.(string)
			if mediaType != "image" && mediaType != "video" && mediaType != "audio" {
				return reject("invalid reference media type")
			}
		}
	}
	expectedFields := 1
	if async && source["mediaType"] != nil {
		expectedFields++
	}
	if len(source) != expectedFields {
		return reject("invalid video reference upload")
	}
	maxBytes := min(limit, 20<<20)
	if async {
		maxBytes = min(limit, 64<<20)
		if mediaType == "image" {
			maxBytes = min(limit, 16<<20)
		}
	}
	var data []byte
	filename := "reference"
	cacheKey := ""
	if ref, ok := source["fileRef"].(string); ok && form != nil {
		field, index, valid := pluginruntime.ParseFileReference(ref)
		if !valid || index >= len(form.File[field]) {
			return reject("unknown video reference file")
		}
		header := form.File[field][index]
		cacheKey = mediaType + ":" + ref
		if uploaded := u.urls[cacheKey]; uploaded != "" {
			return uploaded, nil
		}
		if header.Size > maxBytes || header.Size <= 0 {
			return reject(fmt.Sprintf("reference %s must be non-empty and within the %d byte upload limit", mediaType, maxBytes))
		}
		file, err := header.Open()
		if err != nil {
			return reject("could not read reference image")
		}
		data, err = io.ReadAll(io.LimitReader(file, maxBytes+1))
		file.Close()
		if err != nil {
			return reject("could not read reference image")
		}
		filename = header.Filename
	} else if dataURL, ok := source["dataUrl"].(string); ok {
		header, encoded, valid := strings.Cut(dataURL, ",")
		if !valid || !strings.HasPrefix(strings.ToLower(header), "data:"+mediaType+"/") || !strings.HasSuffix(strings.ToLower(header), ";base64") {
			return reject("reference media must be a base64 data URL of the expected type")
		}
		cacheKey = mediaType + ":" + fmt.Sprintf("%x", sha256.Sum256([]byte(dataURL)))
		if uploaded := u.urls[cacheKey]; uploaded != "" {
			return uploaded, nil
		}
		if len(encoded) > base64.StdEncoding.EncodedLen(int(maxBytes)) {
			return reject(fmt.Sprintf("reference %s exceeds the %d byte upload limit", mediaType, maxBytes))
		}
		var err error
		data, err = base64.StdEncoding.Strict().DecodeString(encoded)
		if err != nil {
			return reject("invalid reference image base64")
		}
	} else {
		return reject("invalid video reference upload source")
	}
	if len(data) == 0 || int64(len(data)) > maxBytes || *total+int64(len(data)) > limit {
		return reject("reference images exceed the upload size limit")
	}
	if len(u.urls) >= dto.MaxImageN {
		return reject("too many reference image uploads in one request")
	}
	contentType := http.DetectContentType(data)
	if !strings.HasPrefix(contentType, mediaType+"/") {
		return reject("reference file must contain " + mediaType + " media")
	}
	*total += int64(len(data))
	if sd {
		link, err := service.StoreVideoReference(c.Request.Context(), data)
		if err != nil {
			return "", &videoReferenceError{message: err.Error(), status: http.StatusBadRequest}
		}
		u.urls[cacheKey] = link
		return link, nil
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	model := u.info.UpstreamModelName
	if model == "" {
		model = u.info.OriginModelName
	}
	if !async {
		if err := writer.WriteField("model", model); err != nil {
			return "", err
		}
		if err := writer.WriteField("purpose", "video"); err != nil {
			return "", err
		}
	}
	partHeader := make(textproto.MIMEHeader)
	partHeader.Set("Content-Disposition", multipart.FileContentDisposition("file", filename))
	partHeader.Set("Content-Type", contentType)
	part, err := writer.CreatePart(partHeader)
	if err != nil {
		return "", err
	}
	if _, err = part.Write(data); err != nil {
		return "", err
	}
	if err = writer.Close(); err != nil {
		return "", err
	}
	baseURL := strings.TrimRight(u.info.ChannelBaseUrl, "/")
	if async {
		baseURL = strings.TrimSuffix(baseURL, "/v1")
	}
	uploadURL := baseURL + "/v1/files"
	if err := pluginruntime.ValidateRequestURL(uploadURL, u.info.ChannelBaseUrl, nil); err != nil {
		return reject("invalid reference upload endpoint")
	}
	client, err := service.GetHttpClientWithProxySettings(u.info.ChannelSetting.Proxy, u.info.ChannelSetting)
	if err != nil {
		return reject("invalid reference upload proxy")
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, &body)
	if err != nil {
		return reject("invalid reference upload request")
	}
	request.Header.Set("Authorization", "Bearer "+u.info.ApiKey)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	// The upload receives channel credentials, so it must never follow redirects.
	noRedirects := *client
	noRedirects.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := noRedirects.Do(request)
	if err != nil {
		return "", &videoReferenceError{message: "reference image upload failed", status: http.StatusBadGateway}
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		status := http.StatusBadRequest
		if response.StatusCode >= 500 || response.StatusCode == http.StatusTooManyRequests {
			status = http.StatusBadGateway
		}
		return "", &videoReferenceError{message: fmt.Sprintf("reference image upload failed (HTTP %d)", response.StatusCode), status: status}
	}
	reply, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(reply) > 1<<20 {
		return reject("invalid reference image upload response")
	}
	var result struct {
		URL    string `json:"url"`
		ID     string `json:"id"`
		FileID string `json:"file_id"`
		Data   struct {
			URL string `json:"url"`
			ID  string `json:"id"`
		} `json:"data"`
	}
	if common.Unmarshal(reply, &result) != nil {
		return reject("invalid reference image upload response")
	}
	if async {
		id := result.ID
		if id == "" {
			id = result.FileID
		}
		if id == "" {
			id = result.Data.ID
		}
		if !generationFileID.MatchString(id) {
			return reject("reference upload did not return a valid file ID")
		}
		u.urls[cacheKey] = id
		return id, nil
	}
	if result.URL == "" {
		result.URL = result.Data.URL
	}
	parsed, err := url.Parse(result.URL)
	if err != nil || len(result.URL) > 8192 || parsed.User != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return reject("reference image upload did not return an HTTP or HTTPS URL")
	}
	u.urls[cacheKey] = result.URL
	return result.URL, nil
}
