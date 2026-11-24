package jsplugin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"math"
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
	"github.com/abema/go-mp4"
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
var videoMediaID = regexp.MustCompile(`^med_[A-Za-z0-9_-]{1,200}$`)

func measureVideoUpscaleSource(c *gin.Context, value any) (float64, error) {
	body, _ := value.(map[string]any)
	references, ok := body["input_videos"].([]any)
	if !ok || len(references) != 1 {
		return 0, fmt.Errorf("video upscaling requires exactly one source video")
	}
	reference, _ := references[0].(map[string]any)
	placeholder, _ := reference["media_id"].(map[string]any)
	source, _ := placeholder["__videoUpload"].(map[string]any)
	limit := min(maxInlineFileBytes(), int64(64<<20))
	var data []byte
	if ref, ok := source["fileRef"].(string); ok {
		form, err := common.ParseMultipartFormReusable(c)
		if err != nil {
			return 0, fmt.Errorf("could not read source video")
		}
		defer form.RemoveAll()
		field, index, valid := pluginruntime.ParseFileReference(ref)
		if !valid || index >= len(form.File[field]) {
			return 0, fmt.Errorf("unknown source video file")
		}
		header := form.File[field][index]
		if header.Size <= 0 || header.Size > limit {
			return 0, fmt.Errorf("source video exceeds the upload size limit")
		}
		file, err := header.Open()
		if err != nil {
			return 0, fmt.Errorf("could not read source video")
		}
		defer file.Close()
		data, err = io.ReadAll(io.LimitReader(file, limit+1))
		if err != nil {
			return 0, fmt.Errorf("could not read source video")
		}
	} else if value, ok := source["dataUrl"].(string); ok {
		header, encoded, valid := strings.Cut(value, ",")
		if !valid || strings.ToLower(header) != "data:video/mp4;base64" || len(encoded) > base64.StdEncoding.EncodedLen(int(limit)) {
			return 0, fmt.Errorf("source video must be an MP4 base64 data URL within the upload limit")
		}
		var err error
		data, err = base64.StdEncoding.Strict().DecodeString(encoded)
		if err != nil {
			return 0, fmt.Errorf("invalid source video base64")
		}
	} else {
		return 0, fmt.Errorf("attach an MP4 source video")
	}
	if len(data) == 0 || int64(len(data)) > limit {
		return 0, fmt.Errorf("source video exceeds the upload size limit")
	}
	if http.DetectContentType(data) != "video/mp4" {
		return 0, fmt.Errorf("source video must contain MP4 video media")
	}
	facts, err := measureMP4Video(data)
	if err != nil {
		return 0, fmt.Errorf("source video must contain readable MP4 duration and dimensions")
	}
	seconds, _ := facts["seconds"].(float64)
	var movieTimescale uint32
	var tracks []struct {
		video         bool
		duration      uint64
		mediaDuration uint64
		timescale     uint32
	}
	visited := 0
	_, err = mp4.ReadBoxStructure(bytes.NewReader(data), func(handle *mp4.ReadHandle) (any, error) {
		visited++
		if visited > 512 {
			return nil, fmt.Errorf("source video metadata exceeds the box limit")
		}
		depth, kind := len(handle.Path), handle.BoxInfo.Type
		if depth == 2 && kind == mp4.BoxTypeTrak() {
			tracks = append(tracks, struct {
				video         bool
				duration      uint64
				mediaDuration uint64
				timescale     uint32
			}{})
			return handle.Expand()
		}
		if (depth == 1 && kind == mp4.BoxTypeMoov()) || (depth == 3 && kind == mp4.BoxTypeMdia()) {
			return handle.Expand()
		}
		if depth == 2 && kind == mp4.BoxTypeMvhd() || depth == 3 && kind == mp4.BoxTypeTkhd() || depth == 4 && (kind == mp4.BoxTypeMdhd() || kind == mp4.BoxTypeHdlr()) {
			if handle.BoxInfo.Size > 4096 {
				return nil, fmt.Errorf("source video metadata header is too large")
			}
			box, _, err := handle.ReadPayload()
			if err != nil {
				return nil, err
			}
			if movie, ok := box.(*mp4.Mvhd); ok {
				if movieTimescale != 0 || movie.Timescale == 0 {
					return nil, fmt.Errorf("source video movie timescale is invalid")
				}
				movieTimescale = movie.Timescale
				seconds = max(seconds, float64(movie.GetDuration())/float64(movie.Timescale))
			} else if len(tracks) > 0 {
				track := &tracks[len(tracks)-1]
				switch header := box.(type) {
				case *mp4.Tkhd:
					track.duration = header.GetDuration()
				case *mp4.Mdhd:
					track.mediaDuration, track.timescale = header.GetDuration(), header.Timescale
				case *mp4.Hdlr:
					track.video = header.HandlerType == [4]byte{'v', 'i', 'd', 'e'}
				}
			}
		}
		return nil, nil
	})
	if err != nil || movieTimescale == 0 {
		return 0, fmt.Errorf("source video must contain readable MP4 movie and track headers")
	}
	video := false
	for _, track := range tracks {
		if !track.video {
			continue
		}
		video = true
		if track.timescale == 0 {
			return 0, fmt.Errorf("source video track timescale is invalid")
		}
		// One short movie or track header must not reduce the source reservation.
		seconds = max(seconds, float64(track.duration)/float64(movieTimescale), float64(track.mediaDuration)/float64(track.timescale))
	}
	if !video {
		return 0, fmt.Errorf("source video must contain a video track")
	}
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 || seconds > relaycommon.MaxTaskDurationSeconds {
		return 0, fmt.Errorf("source video duration must be greater than zero and no greater than 3600 seconds")
	}
	return math.Ceil(seconds), nil
}

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
	cCompatible := u.adaptor.plugin.Meta.Key == "c-compatible"
	if !u.info.HasChannelMeta() || (!async && !sd && !cCompatible && (u.adaptor.plugin.Meta.Key != "sora" || u.info.ChannelSetting.VideoRequestFormat != "json")) {
		return reject("video reference uploads require the channel JSON video format")
	}
	source, ok := placeholder["__videoUpload"].(map[string]any)
	if len(placeholder) != 1 || !ok {
		return reject("invalid video reference upload")
	}
	mediaType := "image"
	if async || cCompatible {
		if value, exists := source["mediaType"]; exists {
			mediaType, _ = value.(string)
			if mediaType != "image" && mediaType != "video" && mediaType != "audio" {
				return reject("invalid reference media type")
			}
		}
	}
	expectedFields := 1
	if (async || cCompatible) && source["mediaType"] != nil {
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
	if cCompatible && mediaType != "image" {
		maxBytes = min(limit, 64<<20)
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
	if cCompatible && mediaType == "audio" && len(data) >= 12 && bytes.Equal(data[4:8], []byte("ftyp")) {
		// MP4 MIME sniffing cannot distinguish M4A audio from a video container.
		audio, video, visited := false, false, 0
		_, err := mp4.ReadBoxStructure(bytes.NewReader(data), func(handle *mp4.ReadHandle) (any, error) {
			visited++
			if visited > 512 {
				return nil, fmt.Errorf("reference audio metadata exceeds the box limit")
			}
			depth, kind := len(handle.Path), handle.BoxInfo.Type
			if (depth == 1 && kind == mp4.BoxTypeMoov()) || (depth == 2 && kind == mp4.BoxTypeTrak()) || (depth == 3 && kind == mp4.BoxTypeMdia()) {
				return handle.Expand()
			}
			if depth == 4 && kind == mp4.BoxTypeHdlr() {
				if handle.BoxInfo.Size > 4096 {
					return nil, fmt.Errorf("reference audio metadata header is too large")
				}
				box, _, err := handle.ReadPayload()
				if err != nil {
					return nil, err
				}
				handler := box.(*mp4.Hdlr).HandlerType
				audio = audio || handler == [4]byte{'s', 'o', 'u', 'n'}
				video = video || handler == [4]byte{'v', 'i', 'd', 'e'}
			}
			return nil, nil
		})
		if err == nil && audio && !video {
			contentType = "audio/mp4"
		} else {
			return reject("reference audio must contain audio tracks without video")
		}
	}
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
	if !async && !cCompatible {
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
	if async || cCompatible {
		baseURL = strings.TrimSuffix(baseURL, "/v1")
	}
	uploadURL := baseURL + "/v1/files"
	if cCompatible {
		uploadURL = baseURL + "/v1/media/uploads"
	}
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
		URL     string `json:"url"`
		ID      string `json:"id"`
		FileID  string `json:"file_id"`
		MediaID string `json:"media_id"`
		Data    struct {
			URL string `json:"url"`
			ID  string `json:"id"`
		} `json:"data"`
	}
	if common.Unmarshal(reply, &result) != nil {
		return reject("invalid reference image upload response")
	}
	if cCompatible {
		if !videoMediaID.MatchString(result.MediaID) {
			return reject("reference upload did not return a valid media ID")
		}
		u.urls[cacheKey] = result.MediaID
		return result.MediaID, nil
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
