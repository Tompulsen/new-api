package common

import (
	"bytes"
	"io"
	"maps"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSanitizeURLForLogMasksSensitiveQueryValues(t *testing.T) {
	rawURL := "https://example.test/v1beta/models/gemini:streamGenerateContent?alt=sse&key=sk-secret&access_token=ya29-secret&api-version=2024-02-01"

	got := SanitizeURLForLog(rawURL)

	assert.NotContains(t, got, "sk-secret")
	assert.NotContains(t, got, "ya29-secret")
	parsedURL, err := url.Parse(got)
	require.NoError(t, err)
	query := parsedURL.Query()
	assert.Equal(t, "***masked***", query.Get("key"))
	assert.Equal(t, "***masked***", query.Get("access_token"))
	assert.Equal(t, "sse", query.Get("alt"))
	assert.Equal(t, "2024-02-01", query.Get("api-version"))
}

func TestSanitizeURLForLogMasksAWSAndSecretLikeQueryKeys(t *testing.T) {
	rawURL := "https://example.test/path?X-Amz-Credential=credential&X-Amz-Signature=signature&session_token=session&client_secret=secret&model=gpt-test"

	got := SanitizeURLForLog(rawURL)

	assert.NotContains(t, got, "X-Amz-Credential=credential")
	assert.NotContains(t, got, "X-Amz-Signature=signature")
	assert.NotContains(t, got, "session_token=session")
	assert.NotContains(t, got, "client_secret=secret")
	parsedURL, err := url.Parse(got)
	require.NoError(t, err)
	query := parsedURL.Query()
	assert.Equal(t, "***masked***", query.Get("X-Amz-Credential"))
	assert.Equal(t, "***masked***", query.Get("X-Amz-Signature"))
	assert.Equal(t, "***masked***", query.Get("session_token"))
	assert.Equal(t, "***masked***", query.Get("client_secret"))
	assert.Equal(t, "gpt-test", query.Get("model"))
}

func TestSanitizeURLForLogKeepsURLWithoutSensitiveQuery(t *testing.T) {
	rawURL := "https://example.test/v1/chat/completions?api-version=2024-02-01&alt=sse"

	got := SanitizeURLForLog(rawURL)

	assert.Equal(t, rawURL, got)
}

func TestValidateMultipartDirectNormalizesImageField(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := strings.NewReader(`{"model":"wan2.7-i2v","prompt":"animate","image":" https://example.com/first.png "}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/video/generations", body)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = request
	info := &RelayInfo{
		TaskRelayInfo: &TaskRelayInfo{},
	}

	taskErr := ValidateMultipartDirect(context, info)

	require.Nil(t, taskErr)
	storedReq, err := GetTaskRequest(context)
	require.NoError(t, err)
	require.Equal(t, []string{"https://example.com/first.png"}, storedReq.Images)
	require.Equal(t, constant.TaskActionImageToVideo, info.Action)
}

func TestBasicTaskRequestPreservesDurationFields(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name      string
		fields    map[string]string
		want      map[string]any
		reference bool
		wantError bool
	}{
		{name: "seconds without duration", fields: map[string]string{"seconds": "6"}, want: map[string]any{"seconds": "6"}},
		{name: "seconds with reference", fields: map[string]string{"seconds": "6"}, want: map[string]any{"seconds": "6"}, reference: true},
		{name: "explicit duration", fields: map[string]string{"duration": "8"}, want: map[string]any{"duration": float64(8)}},
		{name: "both explicit fields", fields: map[string]string{"seconds": "6", "duration": "8"}, want: map[string]any{"seconds": "6", "duration": float64(8)}},
		{name: "no duration fields", want: map[string]any{}},
		{name: "maximum seconds", fields: map[string]string{"seconds": "3600"}, want: map[string]any{"seconds": "3600"}},
		{name: "seconds above limit", fields: map[string]string{"seconds": "3601"}, wantError: true},
		{name: "negative seconds", fields: map[string]string{"seconds": "-1"}, wantError: true},
		{name: "duration above limit", fields: map[string]string{"duration": "3601"}, wantError: true},
	} {
		for _, format := range []string{"json", "multipart"} {
			if tc.reference && format == "json" {
				continue
			}
			t.Run(tc.name+"/"+format, func(t *testing.T) {
				fields := map[string]string{"model": "custom-video", "prompt": "animate"}
				maps.Copy(fields, tc.fields)
				var body bytes.Buffer
				contentType := "application/json"
				if format == "multipart" {
					writer := multipart.NewWriter(&body)
					for key, value := range fields {
						require.NoError(t, writer.WriteField(key, value))
					}
					if tc.reference {
						file, err := writer.CreateFormFile("input_reference", "reference.png")
						require.NoError(t, err)
						_, err = io.WriteString(file, "reference-bytes")
						require.NoError(t, err)
					}
					require.NoError(t, writer.Close())
					contentType = writer.FormDataContentType()
				} else {
					encoded, err := common.Marshal(fields)
					require.NoError(t, err)
					_, err = body.Write(encoded)
					require.NoError(t, err)
				}
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", &body)
				c.Request.Header.Set("Content-Type", contentType)
				c.Set(common.KeyRequestBody, append([]byte(nil), body.Bytes()...))
				t.Cleanup(func() {
					common.CleanupBodyStorage(c)
					if c.Request.MultipartForm != nil {
						require.NoError(t, c.Request.MultipartForm.RemoveAll())
					}
				})
				info := &RelayInfo{TaskRelayInfo: &TaskRelayInfo{}}
				taskErr := ValidateBasicTaskRequest(c, info, constant.TaskActionImageToVideo)
				if tc.wantError {
					require.NotNil(t, taskErr)
					assert.Equal(t, "invalid_seconds", taskErr.Code)
					return
				}
				require.Nil(t, taskErr)
				request, err := GetTaskRequest(c)
				require.NoError(t, err)
				encoded, err := common.Marshal(request)
				require.NoError(t, err)
				var forwarded map[string]any
				require.NoError(t, common.Unmarshal(encoded, &forwarded))
				expected := map[string]any{"model": "custom-video", "prompt": "animate"}
				maps.Copy(expected, tc.want)
				assert.Equal(t, expected, forwarded)
				if tc.reference {
					form, err := common.ParseMultipartFormReusable(c)
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, form.RemoveAll()) })
					require.Len(t, form.File["input_reference"], 1)
					file, err := form.File["input_reference"][0].Open()
					require.NoError(t, err)
					defer file.Close()
					content, err := io.ReadAll(file)
					require.NoError(t, err)
					assert.Equal(t, "reference-bytes", string(content))
				}
			})
		}
	}
}

// TestTaskDurationBounds guards the billing invariant that user-supplied
// video duration (a quota multiplier via OtherRatio "seconds") is bounded, so
// it can never overflow quota calculation into a negative charge.
func TestTaskDurationBounds(t *testing.T) {
	gin.SetMode(gin.TestMode)

	newContext := func(t *testing.T, body string) (*gin.Context, *RelayInfo) {
		request := httptest.NewRequest(http.MethodPost, "/v1/video/generations", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		context, _ := gin.CreateTestContext(httptest.NewRecorder())
		context.Request = request
		return context, &RelayInfo{TaskRelayInfo: &TaskRelayInfo{}}
	}

	tests := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{
			name:    "huge duration is rejected",
			body:    `{"model":"sora-2","prompt":"a cat","duration":9999999999}`,
			wantErr: true,
		},
		{
			name:    "huge seconds string is rejected",
			body:    `{"model":"sora-2","prompt":"a cat","seconds":"9999999999"}`,
			wantErr: true,
		},
		{
			name:    "negative duration is rejected",
			body:    `{"model":"sora-2","prompt":"a cat","duration":-8}`,
			wantErr: true,
		},
		{
			name: "normal duration is accepted",
			body: `{"model":"sora-2","prompt":"a cat","seconds":"8"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name+" (multipart direct)", func(t *testing.T) {
			context, info := newContext(t, tt.body)
			taskErr := ValidateMultipartDirect(context, info)
			if tt.wantErr {
				require.NotNil(t, taskErr)
				require.Equal(t, "invalid_seconds", taskErr.Code)
			} else {
				require.Nil(t, taskErr)
			}
		})
		t.Run(tt.name+" (basic task request)", func(t *testing.T) {
			context, info := newContext(t, tt.body)
			taskErr := ValidateBasicTaskRequest(context, info, constant.TaskActionImageToVideo)
			if tt.wantErr {
				require.NotNil(t, taskErr)
				require.Equal(t, "invalid_seconds", taskErr.Code)
			} else {
				require.Nil(t, taskErr)
			}
		})
	}
}
