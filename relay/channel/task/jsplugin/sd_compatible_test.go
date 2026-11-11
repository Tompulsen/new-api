package jsplugin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/plugins"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSDCompatibleVideoContract(t *testing.T) {
	source, err := plugins.Source("sd-compatible")
	require.NoError(t, err)
	registry := pluginruntime.NewRegistry()
	plugin, err := registry.RegisterFactory(source, pluginruntime.Options{Key: "sd-compatible"})
	require.NoError(t, err)
	assert.Equal(t, "SD兼容", plugin.Meta.Name)
	for _, name := range plugin.Meta.Models {
		binding, found := registry.Generation().LookupEndpoint("POST", "/v1/videos", name)
		require.True(t, found)
		assert.Same(t, plugin, binding.Plugin)
	}
	ctx := map[string]any{
		"baseUrl": "https://upstream.example/v1/", "apiKey": "selected-key",
		"model": "client-alias", "upstreamModel": "sd2.0-mini-720p",
		"requestBody": map[string]any{
			"prompt": "waves", "seconds": "5", "size": "720x1280",
			"input_reference": map[string]any{"image_url": "https://images.example/ref.png?signature=a%2Bb"},
			"metadata":        map[string]any{"duration": 10, "resolution": "720p", "aspect_ratio": "9:16"},
		},
	}
	var descriptor requestDescriptor
	require.NoError(t, plugin.Engine.CallInto(t.Context(), &descriptor, "buildSubmitRequest", ctx))
	assert.Equal(t, "https://upstream.example/v1/tasks/sora", descriptor.URL)
	assert.Equal(t, "application/json", descriptor.Headers["Content-Type"])
	assert.Equal(t, "Bearer selected-key", descriptor.Headers["Authorization"])
	assert.Equal(t, map[string]any{
		"model": "sd2.0-mini-720p", "prompt": "waves", "seconds": "5", "ratio": "9:16", "aspect_ratio": "9:16", "resolution": "720p",
		"images": []any{"https://images.example/ref.png?signature=a%2Bb"},
	}, descriptor.Body)
	var usage map[string]any
	require.NoError(t, plugin.Engine.CallInto(t.Context(), &usage, "extractUsage", ctx))
	assert.Equal(t, map[string]any{"seconds": float64(5), "resolution": "720p"}, usage)

	t.Run("default resolution follows mapped model and body facts", func(t *testing.T) {
		ctx["upstreamModel"] = "sd2.0-fast-480p"
		ctx["requestBody"] = map[string]any{"prompt": "waves", "metadata": map[string]any{"aspect_ratio": "auto"}}
		require.NoError(t, plugin.Engine.CallInto(t.Context(), &descriptor, "buildSubmitRequest", ctx))
		body := descriptor.Body.(map[string]any)
		assert.Equal(t, "480p", body["resolution"])
		assert.Equal(t, "adaptive", body["ratio"])
		assert.Equal(t, "5", body["seconds"])
		assert.NotContains(t, body, "images")
		require.NoError(t, plugin.Engine.CallInto(t.Context(), &usage, "extractUsage", ctx))
		assert.Equal(t, map[string]any{"seconds": float64(5), "resolution": "480p"}, usage)
	})
	t.Run("root aliases precede metadata and references are unique", func(t *testing.T) {
		ctx["requestBody"] = map[string]any{
			"prompt": "waves", "duration": 10, "ratio": "1:1", "resolution": "1080P",
			"images": []any{"https://images.example/first.png", "https://images.example/last.png"},
			"metadata": map[string]any{"seconds": 30, "resolution": "480p", "aspect_ratio": "9:16", "content": []any{
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://images.example/first.png"}, "role": "first_frame"},
			}},
		}
		require.NoError(t, plugin.Engine.CallInto(t.Context(), &descriptor, "buildSubmitRequest", ctx))
		body := descriptor.Body.(map[string]any)
		assert.Equal(t, "1080p", body["resolution"])
		assert.Equal(t, "1:1", body["ratio"])
		assert.Equal(t, "10", body["seconds"])
		assert.Equal(t, []any{"https://images.example/first.png", "https://images.example/last.png"}, body["images"])
		assert.NotContains(t, body, "metadata")
	})
	t.Run("frame roles preserve order when the last frame also appears among references", func(t *testing.T) {
		ctx["requestBody"] = map[string]any{
			"prompt":     "waves",
			"images":     []any{"https://images.example/middle.png", "https://images.example/last.png", "https://images.example/extra.png"},
			"last_frame": "https://images.example/last.png",
			"metadata": map[string]any{"content": []any{
				map[string]any{"type": "image_url", "image_url": "https://images.example/last.png", "role": "last_frame"},
				map[string]any{"type": "image_url", "image_url": "https://images.example/middle.png"},
				map[string]any{"type": "image_url", "image_url": "https://images.example/first.png", "role": "first_frame"},
			}},
		}
		require.NoError(t, plugin.Engine.CallInto(t.Context(), &descriptor, "buildSubmitRequest", ctx))
		assert.Equal(t, []any{
			"https://images.example/first.png", "https://images.example/middle.png", "https://images.example/extra.png", "https://images.example/last.png",
		}, descriptor.Body.(map[string]any)["images"])
	})
	for _, invalid := range []map[string]any{
		{"seconds": 0}, {"seconds": -1}, {"seconds": 3601}, {"seconds": 5.5}, {"duration": true},
		{"metadata": map[string]any{"duration": 1e30}},
		{"images": []any{"data:image/png;base64,AAAA"}},
		{"input_reference": "file_abcdefghijklmnop"}, {"images": []any{"blob:private"}},
		{"images": []any{"https://user:secret@private.example/ref.png"}},
		{"images": []any{map[string]any{"__videoUpload": map[string]any{"dataUrl": "private"}}}},
		{"metadata": "invalid"}, {"images": "not an array"},
	} {
		invalid["prompt"] = "waves"
		ctx["requestBody"] = invalid
		_, err := plugin.Engine.Call(t.Context(), "buildSubmitRequest", ctx)
		require.Error(t, err, "%v", invalid)
	}
	t.Run("multipart URL references decode without keeping the multipart wire format", func(t *testing.T) {
		var intent map[string]any
		require.NoError(t, plugin.Engine.CallPathInto(t.Context(), &intent, "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
			"model": "client-alias", "body": map[string]any{
				"kind": "multipart", "fields": map[string]any{
					"prompt": []any{"waves"}, "seconds": []any{"5"},
					"metadata": []any{`{"ratio":"9:16","resolution":"720p","images":["https://images.example/ref.png"]}`},
				},
			},
		}))
		assert.Equal(t, "client-alias", intent["model"])
		assert.Equal(t, "image_to_video", intent["action"])
	})
	t.Run("local attachments reject before generation or upload", func(t *testing.T) {
		c := newMultipartFileContext(t, "input_reference", "ref.png", "image/png", []byte("image"))
		c.Set("task_request", map[string]any{"prompt": "waves", "seconds": "5"})
		info := &relaycommon.RelayInfo{
			ChannelMeta:   &relaycommon.ChannelMeta{ChannelBaseUrl: "https://must-not-be-contacted.example", ApiKey: "selected-key"},
			TaskRelayInfo: &relaycommon.TaskRelayInfo{},
		}
		info.OriginModelName, info.UpstreamModelName = "sd2.0-mini-720p", "sd2.0-mini-720p"
		adaptor := New(plugin)
		adaptor.Init(info)
		failure := adaptor.ValidateRequestAndSetAction(c, info)
		require.NotNil(t, failure)
		assert.Equal(t, http.StatusBadRequest, failure.StatusCode)
		assert.Contains(t, failure.Message, "public HTTP(S) image URLs")
	})
	t.Run("query status failure details and missing IDs", func(t *testing.T) {
		require.NoError(t, plugin.Engine.CallInto(t.Context(), &descriptor, "buildQueryRequest", map[string]any{
			"baseUrl": "https://upstream.example", "apiKey": "selected-key", "taskId": "id/with space",
		}))
		assert.Equal(t, "https://upstream.example/v1/videos/id%2Fwith%20space", descriptor.URL)
		for upstream, want := range map[string]string{
			"queued": "QUEUED", "in_progress": "IN_PROGRESS", "completed": "SUCCESS", "failed": "FAILURE", "": "UNKNOWN", "new-state": "UNKNOWN",
		} {
			var result map[string]any
			require.NoError(t, plugin.Engine.CallInto(t.Context(), &result, "parseTaskResult", nil, map[string]any{
				"status": upstream, "error": map[string]any{"message": "provider rejection"},
			}))
			assert.Equal(t, want, result["status"])
			if want == "FAILURE" {
				assert.Equal(t, "provider rejection", result["reason"])
			}
		}
		var result map[string]any
		require.NoError(t, plugin.Engine.CallInto(t.Context(), &result, "parseSubmitResponse", nil, map[string]any{"body": map[string]any{"task_id": "upstream-id"}}))
		assert.Equal(t, "upstream-id", result["taskId"])
		_, err := plugin.Engine.Call(t.Context(), "parseSubmitResponse", nil, map[string]any{"body": map[string]any{}})
		require.ErrorContains(t, err, "missing a task ID")
	})
	t.Run("completion uses actual dimensions and preserves signed artifact URLs without credentials", func(t *testing.T) {
		require.NoError(t, plugin.Engine.CallInto(t.Context(), &usage, "extractUsageOnComplete", nil, nil, map[string]any{
			"seconds": "6.5", "resolution": "480p", "width": 1920, "height": 1080,
		}))
		assert.Equal(t, map[string]any{"seconds": 6.5, "resolution": "1080p"}, usage)
		const url = "https://cdn.example/video.mp4?signature=a%2Bb%3D&expires=123"
		var content requestDescriptor
		require.NoError(t, plugin.Engine.CallInto(t.Context(), &content, "buildContentRequest", map[string]any{
			"artifactKey": "video", "data": map[string]any{"result": map[string]any{"video_url": url}},
			"clientRequest": map[string]any{"method": "HEAD"},
		}))
		assert.Equal(t, url, content.URL)
		assert.Equal(t, "HEAD", content.Method)
		assert.True(t, content.Credentialless)
		assert.Empty(t, content.Headers)
		data, err := common.Marshal(content)
		require.NoError(t, err)
		assert.NotContains(t, string(data), "selected-key")
	})
}

func TestSDCompatibleSubmissionUsesNativeJSON(t *testing.T) {
	source, err := plugins.Source("sd-compatible")
	require.NoError(t, err)
	plugin, err := pluginruntime.NewRegistry().RegisterFactory(source, pluginruntime.Options{Key: "sd-compatible"})
	require.NoError(t, err)
	c := newMultipartFileContext(t, "input_reference", "ref.png", "image/png", nil)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader("{}"))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("task_request", map[string]any{"prompt": "waves", "seconds": "5", "metadata": map[string]any{
		"aspect_ratio": "9:16", "resolution": "720p", "images": []any{"https://images.example/ref.png"},
	}})
	info := &relaycommon.RelayInfo{
		ChannelMeta:   &relaycommon.ChannelMeta{ChannelBaseUrl: "https://upstream.example", ApiKey: "selected-key"},
		TaskRelayInfo: &relaycommon.TaskRelayInfo{},
	}
	info.OriginModelName, info.UpstreamModelName = "client-alias", "sd2.0-mini-720p"
	adaptor := New(plugin)
	adaptor.Init(info)
	body, err := adaptor.BuildRequestBody(c, info)
	require.NoError(t, err)
	var actual map[string]any
	require.NoError(t, common.DecodeJson(body, &actual))
	assert.Equal(t, "sd2.0-mini-720p", actual["model"])
	assert.Equal(t, "5", actual["seconds"])
	assert.Equal(t, "9:16", actual["ratio"])
	assert.Equal(t, []any{"https://images.example/ref.png"}, actual["images"])
	assert.NotContains(t, actual, "metadata")
	assert.NotContains(t, actual, "input_reference")
}
