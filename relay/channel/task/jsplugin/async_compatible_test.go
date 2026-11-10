package jsplugin

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/plugins"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAsyncCompatibleVideoContract(t *testing.T) {
	source, err := plugins.Source("async-compatible")
	require.NoError(t, err)
	registry := pluginruntime.NewRegistry()
	plugin, err := registry.RegisterFactory(source, pluginruntime.Options{Key: "async-compatible"})
	require.NoError(t, err)
	assert.Equal(t, "异步兼容", plugin.Meta.Name)
	for _, model := range plugin.Meta.Models {
		binding, found := registry.Generation().LookupEndpoint("POST", "/v1/videos", model)
		require.True(t, found)
		assert.Same(t, plugin, binding.Plugin)
	}
	ctx := map[string]any{
		"baseUrl": "https://upstream.example/v1", "apiKey": "selected-key",
		"publicTaskId": "task-public-123", "model": "client-alias", "upstreamModel": "future-video-model",
		"requestBody": map[string]any{
			"prompt": "waves", "seconds": "6", "size": "854x480", "generate_audio": false,
			"metadata": map[string]any{"duration": 30, "generate_audio": true, "aspect_ratio": "9:16"},
		},
	}
	value, err := plugin.Engine.Call(t.Context(), "buildSubmitRequest", ctx)
	require.NoError(t, err)
	data, err := common.Marshal(value)
	require.NoError(t, err)
	var request requestDescriptor
	require.NoError(t, common.Unmarshal(data, &request))
	assert.Equal(t, "https://upstream.example/v1/generations", request.URL)
	assert.Equal(t, "Bearer selected-key", request.Headers["Authorization"])
	assert.Equal(t, "task-public-123", request.Headers["Idempotency-Key"])
	body := request.Body.(map[string]any)
	assert.Equal(t, "future-video-model", body["model"])
	assert.Equal(t, float64(6), body["duration"])
	assert.Equal(t, "480p", body["resolution"])
	assert.Equal(t, "9:16", body["aspect_ratio"])
	assert.Equal(t, false, body["generate_audio"])
	assert.NotContains(t, body, "seconds")
	assert.NotContains(t, body, "metadata")

	t.Run("root aliases precede metadata and defaults are explicit", func(t *testing.T) {
		ctx["requestBody"] = map[string]any{"prompt": "waves", "duration": 10, "ratio": "1:1", "metadata": map[string]any{"seconds": 30, "aspect_ratio": "9:16"}}
		var descriptor requestDescriptor
		require.NoError(t, plugin.Engine.CallInto(t.Context(), &descriptor, "buildSubmitRequest", ctx))
		actual := descriptor.Body.(map[string]any)
		assert.Equal(t, float64(10), actual["duration"])
		assert.Equal(t, "1:1", actual["aspect_ratio"])
		assert.Equal(t, "720p", actual["resolution"])
		assert.NotContains(t, actual, "reference_image_processing", "paid preprocessing must be opt-in")
		ctx["requestBody"] = map[string]any{"prompt": "waves"}
		require.NoError(t, plugin.Engine.CallInto(t.Context(), &descriptor, "buildSubmitRequest", ctx))
		actual = descriptor.Body.(map[string]any)
		assert.Equal(t, float64(5), actual["duration"])
		assert.Equal(t, "16:9", actual["aspect_ratio"])
		assert.Equal(t, "task-public-123", descriptor.Headers["Idempotency-Key"])
	})
	for _, invalid := range []map[string]any{
		{"seconds": 0},
		{"metadata": map[string]any{"duration": 3601}},
		{"duration": true},
		{"reference_images": []any{"file_abcdefghijklmnop"}},
		{"reference_images": []any{"https://remote.example/ref.png"}},
		{"metadata": map[string]any{"reference_videos": []any{"file_abcdefghijklmnop"}}},
		{"extra": map[string]any{"__videoUpload": map[string]any{"fileRef": "request_file:private"}}},
		{"reference_image_processing": "process"},
	} {
		invalid["prompt"] = "waves"
		ctx["requestBody"] = invalid
		_, err := plugin.Engine.Call(t.Context(), "buildSubmitRequest", ctx)
		require.Error(t, err, "%v", invalid)
	}
	t.Run("both decoders reject unowned upstream files", func(t *testing.T) {
		decoderCtx := map[string]any{"model": "client-alias", "body": map[string]any{
			"kind": "json", "value": map[string]any{"model": "client-alias", "prompt": "waves", "reference_images": []any{"file_abcdefghijklmnop"}},
		}}
		_, err := plugin.Engine.CallPath(t.Context(), "native", []string{"createGeneration"}, decoderCtx)
		require.ErrorContains(t, err, "file IDs cannot be shared")
		_, err = plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, decoderCtx)
		require.ErrorContains(t, err, "file IDs cannot be shared")
	})
	t.Run("multipart decoding retains metadata and repeated attachments", func(t *testing.T) {
		var intent map[string]any
		require.NoError(t, plugin.Engine.CallPathInto(t.Context(), &intent, "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
			"model": "wan3.0", "body": map[string]any{
				"kind": "multipart",
				"fields": map[string]any{
					"prompt": []any{"waves"}, "seconds": []any{"6"},
					"metadata": []any{`{"resolution":"1080p","aspect_ratio":"9:16","generate_audio":false}`},
				},
				"files": []any{
					map[string]any{"field": "input_reference", "ref": "request_file:input_reference"},
					map[string]any{"field": "input_reference", "ref": "request_file:input_reference#1"},
				},
			},
		}))
		assert.Equal(t, "reference_to_video", intent["action"])
		req := intent["requestBody"].(map[string]any)
		assert.Equal(t, "6", req["seconds"])
		assert.Equal(t, false, req["metadata"].(map[string]any)["generate_audio"])
	})
	t.Run("actual frontend Seedance and generic reference payloads", func(t *testing.T) {
		const first = "data:image/png;base64,Zmlyc3Q="
		const last = "data:image/png;base64,bGFzdA=="
		for _, tc := range []struct {
			name, mode string
			body       map[string]any
		}{
			{"Seedance text only", "", map[string]any{"metadata": map[string]any{"content": []any{}, "ratio": "16:9"}}},
			{"Seedance frames", "", map[string]any{"metadata": map[string]any{"ratio": "9:16", "content": []any{
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": first}, "role": "first_frame"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": last}, "role": "last_frame"},
			}}}},
			{"generic references duplicated in metadata", "", map[string]any{
				"images": []any{first, last}, "metadata": map[string]any{"reference_images": []any{map[string]any{"url": first}, map[string]any{"url": last}}},
			}},
			{"generic frames duplicated in metadata", "", map[string]any{
				"images": []any{first, last}, "metadata": map[string]any{"first_frame": map[string]any{"url": first}, "last_frame": map[string]any{"url": last}},
			}},
			{"explicit native frame mode", "first_last_frame", map[string]any{
				"images": []any{first, last}, "reference_mode": "first_last_frame",
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				tc.body["prompt"] = "waves"
				ctx["requestBody"] = tc.body
				var descriptor requestDescriptor
				require.NoError(t, plugin.Engine.CallInto(t.Context(), &descriptor, "buildSubmitRequest", ctx))
				payload := descriptor.Body.(map[string]any)
				assert.NotContains(t, payload, "content")
				assert.NotContains(t, payload, "metadata")
				if tc.name == "Seedance text only" {
					assert.NotContains(t, payload, "reference_images")
					return
				}
				images := payload["reference_images"].([]any)
				require.Len(t, images, 2)
				assert.Equal(t, first, images[0].(map[string]any)["__videoUpload"].(map[string]any)["dataUrl"])
				assert.Equal(t, last, images[1].(map[string]any)["__videoUpload"].(map[string]any)["dataUrl"])
				if tc.mode != "" {
					assert.Equal(t, tc.mode, payload["reference_mode"])
				} else {
					assert.NotContains(t, payload, "reference_mode", "official Seedance endpoints do not accept an inferred mode")
				}
			})
		}
	})
	t.Run("status and signed results", func(t *testing.T) {
		for upstream, want := range map[string]string{
			"created": "QUEUED", "queued": "QUEUED", "processing": "IN_PROGRESS", "review": "IN_PROGRESS",
			"succeeded": "SUCCESS", "failed": "FAILURE", "canceled": "FAILURE", "new-state": "UNKNOWN", "": "UNKNOWN",
		} {
			var parsed struct{ Status string }
			require.NoError(t, plugin.Engine.CallInto(t.Context(), &parsed, "parseTaskResult", ctx, map[string]any{"status": upstream}))
			assert.Equal(t, want, parsed.Status, upstream)
		}
		const signedURL = "https://cdn.example/video.mp4?X-Signature=a%2Bb%3D&expires=123"
		var content requestDescriptor
		require.NoError(t, plugin.Engine.CallInto(t.Context(), &content, "buildContentRequest", map[string]any{
			"artifactKey": "video", "data": map[string]any{"result_url": signedURL}, "clientRequest": map[string]any{"method": "HEAD"},
		}))
		assert.Equal(t, signedURL, content.URL)
		assert.Equal(t, "HEAD", content.Method)
		assert.True(t, content.Credentialless)
		assert.Empty(t, content.Headers)
		var result map[string]any
		require.NoError(t, plugin.Engine.CallInto(t.Context(), &result, "extractUsageOnComplete", nil, nil, map[string]any{
			"duration": 6, "size": "1920x1080", "resolution": "720p", "cost_points": 123456,
		}))
		assert.Equal(t, map[string]any{"seconds": float64(6), "resolution": "1080p"}, result)
	})
}

func TestAsyncCompatibleUploadsBeforeGeneration(t *testing.T) {
	source, err := plugins.Source("async-compatible")
	require.NoError(t, err)
	plugin, err := pluginruntime.NewRegistry().RegisterFactory(source, pluginruntime.Options{Key: "async-compatible"})
	require.NoError(t, err)
	service.InitHttpClient()
	image := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 24)...)
	dataURL := "data:" + "image/png;base64," + base64.StdEncoding.EncodeToString(image)
	for _, tc := range []struct {
		name, input, reply string
		status             int
		wantError          bool
	}{
		{"multipart", "file", `{"id":"file_abcdefghijklmnop"}`, 201, false},
		{"repeated data URL", "data", `{"data":{"id":"file_abcdefghijklmnop"}}`, 200, false},
		{"upstream rejection", "data", `{}`, 400, true},
		{"redirect", "data", `{}`, 307, true},
		{"URL instead of ID", "data", `{"url":"https://cdn.example/ref.png"}`, 200, true},
		{"invalid ID", "data", `{"id":"file_wrong"}`, 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uploads, generations := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "Bearer selected-key", r.Header.Get("Authorization"))
				switch r.URL.Path {
				case "/v1/files":
					uploads++
					require.NoError(t, r.ParseMultipartForm(1<<20))
					defer r.MultipartForm.RemoveAll()
					assert.Empty(t, r.MultipartForm.Value, "the native file endpoint accepts only file")
					file, _, err := r.FormFile("file")
					require.NoError(t, err)
					actual, err := io.ReadAll(file)
					file.Close()
					require.NoError(t, err)
					assert.Equal(t, image, actual)
					w.Header().Set("Location", "/must-not-follow")
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, tc.reply)
				case "/v1/generations":
					generations++
					assert.Equal(t, "task-public-123", r.Header.Get("Idempotency-Key"))
					var actual map[string]any
					require.NoError(t, common.DecodeJson(r.Body, &actual))
					references := actual["reference_images"].([]any)
					for _, value := range references {
						assert.Equal(t, "file_abcdefghijklmnop", value)
					}
					assert.Len(t, references, 1, "the upstream requires unique reference file IDs")
					w.WriteHeader(http.StatusAccepted)
					_, _ = io.WriteString(w, `{"id":"video-123","status":"created"}`)
				default:
					t.Errorf("unexpected upstream request %s", r.URL.Path)
				}
			}))
			defer server.Close()
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			req := map[string]any{"prompt": "waves"}
			if tc.input == "file" {
				c = newMultipartFileContext(t, "input_reference", "ref.png", "image/png", image)
			} else {
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader("{}"))
				c.Request.Header.Set("Content-Type", "application/json")
				req["images"] = []any{dataURL, dataURL}
			}
			c.Set("task_request", req)
			info := &relaycommon.RelayInfo{
				ChannelMeta: &relaycommon.ChannelMeta{
					ChannelType: constant.ChannelTypeTaskPlugin, ChannelBaseUrl: server.URL + "/v1", ApiKey: "selected-key",
				}, TaskRelayInfo: &relaycommon.TaskRelayInfo{PublicTaskID: "task-public-123"},
			}
			info.OriginModelName, info.UpstreamModelName = "alias", "future-model"
			adaptor := New(plugin)
			adaptor.Init(info)
			_, err := adaptor.buildSubmit(c, info)
			require.NoError(t, err)
			assert.Zero(t, uploads, "validation and billing must perform no upload")
			body, err := adaptor.BuildRequestBody(c, info)
			if tc.wantError {
				require.Error(t, err)
				assert.Zero(t, generations)
			} else {
				require.NoError(t, err)
				response, err := adaptor.DoRequest(c, info, body)
				require.NoError(t, err)
				defer response.Body.Close()
				submitted, taskErr := adaptor.ParseResponse(c, response, info)
				require.Nil(t, taskErr, "%+v", taskErr)
				assert.Equal(t, "video-123", submitted.UpstreamTaskID)
				assert.Equal(t, 1, generations)
			}
			assert.Equal(t, 1, uploads, "duplicate references reuse one upload")
		})
	}
	t.Run("media type and host byte bound", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", nil)
		uploader := &videoReferenceUploader{
			adaptor: New(plugin), info: &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}}, urls: map[string]string{},
		}
		for _, source := range []map[string]any{
			{"dataUrl": dataURL, "mediaType": "video"},
			{"dataUrl": dataURL, "mediaType": "image", "endpoint": "https://untrusted.example"},
			{"dataUrl": "data:" + "image/png;base64," + base64.StdEncoding.EncodeToString([]byte("not an image")), "mediaType": "image"},
		} {
			var total int64
			_, err := uploader.resolve(c, map[string]any{"__videoUpload": source}, nil, maxInlineFileBytes(), &total)
			require.Error(t, err)
		}
		var total int64
		_, err := uploader.resolve(c, map[string]any{"__videoUpload": map[string]any{"dataUrl": dataURL}}, nil, int64(len(image)-1), &total)
		require.Error(t, err)
	})
	t.Run("image video and audio uploads retain media order", func(t *testing.T) {
		video := []byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm', 0, 0, 0, 0, 'i', 's', 'o', 'm', 'm', 'p', '4', '2'}
		audio := []byte("ID3\x04\x00\x00\x00\x00\x00\x00")
		uploads := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			uploads++
			require.Equal(t, "/v1/files", r.URL.Path)
			require.NoError(t, r.ParseMultipartForm(1<<20))
			defer r.MultipartForm.RemoveAll()
			file, header, err := r.FormFile("file")
			require.NoError(t, err)
			file.Close()
			assert.Contains(t, []string{"image/png", "video/mp4", "audio/mpeg"}, header.Header.Get("Content-Type"))
			_, _ = io.WriteString(w, `{"file_id":"file_abcdefghijklmnop"}`)
		}))
		defer server.Close()
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader("{}"))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Set("task_request", map[string]any{
			"prompt":           "waves",
			"reference_images": []any{dataURL},
			"reference_videos": []any{"data:" + "video/mp4;base64," + base64.StdEncoding.EncodeToString(video)},
			"reference_audios": []any{"data:" + "audio/mpeg;base64," + base64.StdEncoding.EncodeToString(audio)},
		})
		info := &relaycommon.RelayInfo{
			ChannelMeta:   &relaycommon.ChannelMeta{ChannelBaseUrl: server.URL, ApiKey: "selected-key"},
			TaskRelayInfo: &relaycommon.TaskRelayInfo{PublicTaskID: "task-public-123"},
		}
		info.OriginModelName, info.UpstreamModelName = "wan3.0", "wan3.0"
		adaptor := New(plugin)
		adaptor.Init(info)
		body, err := adaptor.BuildRequestBody(c, info)
		require.NoError(t, err)
		var payload map[string]any
		require.NoError(t, common.DecodeJson(body, &payload))
		for _, field := range []string{"reference_images", "reference_videos", "reference_audios"} {
			assert.Equal(t, []any{"file_abcdefghijklmnop"}, payload[field])
		}
		assert.Equal(t, 3, uploads)
	})
}

func TestAsyncCompatibleMeasuresSignedVideoWithoutCredentials(t *testing.T) {
	source, err := plugins.Source("async-compatible")
	require.NoError(t, err)
	plugin, err := pluginruntime.NewRegistry().RegisterFactory(source, pluginruntime.Options{Key: "async-compatible"})
	require.NoError(t, err)
	settings := system_setting.GetFetchSetting()
	previous := *settings
	settings.EnableSSRFProtection = true
	settings.AllowPrivateIp = true
	settings.AllowedPorts = []string{"1-65535"}
	t.Cleanup(func() { *settings = previous })
	service.InitHttpClient()
	video := videoUsageMP4(t, 1920, 1080)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		assert.Equal(t, "signature=a%2Bb%3D", r.URL.RawQuery)
		assert.Empty(t, r.Header.Get("Authorization"))
		assert.Empty(t, r.Header.Get("X-Channel-Key"))
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write(video)
	}))
	defer server.Close()
	adaptor := New(plugin)
	adaptor.Init(&relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{
		ChannelBaseUrl: "https://upstream.example", ApiKey: "must-not-leak",
	}})
	data, err := common.Marshal(map[string]any{"result_url": server.URL + "/result.mp4?signature=a%2Bb%3D"})
	require.NoError(t, err)
	task := &model.Task{TaskID: "public-video", Data: data}
	task.PrivateData.BillingContext = &model.TaskBillingContext{TieredSnapshot: &billingexpr.BillingSnapshot{
		ExprString:        `u("resolution") == "1080p" ? tier("1080p", u("seconds") * 0.1) : tier("other", u("seconds") * 0.05)`,
		VideoUsagePending: true,
	}}
	result := &relaycommon.TaskInfo{Status: model.TaskStatusSuccess}
	adaptor.completeVideoUsage(t.Context(), task, result)
	assert.Equal(t, 1, requests)
	assert.Equal(t, map[string]any{"seconds": 6.5, "resolution": "1080p"}, result.UsageFacts)
	settings.AllowPrivateIp = false
	blocked := &relaycommon.TaskInfo{Status: model.TaskStatusSuccess}
	adaptor.completeVideoUsage(t.Context(), task, blocked)
	assert.Equal(t, 1, requests, "private addresses must be blocked before downloading")
	assert.Empty(t, blocked.UsageFacts)
}
