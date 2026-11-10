package plugins_test

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	builtinplugins "github.com/QuantumNous/new-api/plugins"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSoraResponsesProtocol(t *testing.T) {
	testVideoResponsesProtocol(t, videoResponsesTestCase{
		pluginKey: "sora",
		model:     "sora-2-pro",
		requestBody: map[string]any{
			"model":   "sora-2-pro",
			"input":   "waves at sunset",
			"seconds": 8,
			"size":    "1792x1024",
		},
		wantAction: "text_to_video",
		wantRequest: map[string]any{
			"model":   "sora-2-pro",
			"prompt":  "waves at sunset",
			"seconds": float64(8),
			"size":    "1792x1024",
		},
		wantUsageKeys:  []string{"seconds", "size"},
		wantVendorName: "sora",
	})
}

func TestSoraChannelJSONVideoRequest(t *testing.T) {
	source, err := builtinplugins.Source("sora")
	require.NoError(t, err)
	plugin, err := jsplugin.NewRegistry().RegisterFactory(source, jsplugin.Options{Key: "sora"})
	require.NoError(t, err)
	ctx := map[string]any{
		"model": "client-alias", "upstreamModel": "vendor-video",
		"baseUrl": "https://video.test", "apiKey": "test-only",
		"videoRequestFormat": "json",
		"files":              []any{map[string]any{"ref": "request_file:input_reference", "field": "input_reference"}},
		"requestBody": map[string]any{
			"prompt": "waves", "seconds": "6", "size": "720x1280",
			"metadata": map[string]any{"resolution": "480p", "aspect_ratio": "3:4", "generate_audio": false},
		},
	}
	var request struct {
		URL, BodyType string
		Headers       map[string]string
		Body          map[string]any
	}
	require.NoError(t, plugin.Engine.CallInto(t.Context(), &request, "buildSubmitRequest", ctx))
	assert.Equal(t, "https://video.test/v1/videos", request.URL)
	assert.Equal(t, "application/json", request.Headers["Content-Type"])
	assert.NotEqual(t, "multipart", request.BodyType)
	assert.Equal(t, "vendor-video", request.Body["model"])
	assert.Equal(t, float64(6), request.Body["duration"])
	assert.Equal(t, "480P", request.Body["resolution"])
	assert.Equal(t, "3:4", request.Body["aspect_ratio"])
	assert.Equal(t, false, request.Body["generate_audio"])
	assert.NotContains(t, request.Body, "seconds")
	assert.NotContains(t, request.Body, "input_reference")
	assert.Equal(t, []any{map[string]any{"__videoUpload": map[string]any{"fileRef": "request_file:input_reference"}}}, request.Body["images"])
	var facts map[string]any
	require.NoError(t, plugin.Engine.CallInto(t.Context(), &facts, "extractUsage", ctx))
	assert.Equal(t, float64(6), facts["seconds"])
	assert.Equal(t, "480p", facts["resolution"])
	ctx["requestBody"].(map[string]any)["metadata"].(map[string]any)["last_frame"] = map[string]any{"url": "https://images.test/last.png"}
	require.NoError(t, plugin.Engine.CallInto(t.Context(), &request, "buildSubmitRequest", ctx))
	assert.Equal(t, []any{
		map[string]any{"__videoUpload": map[string]any{"fileRef": "request_file:input_reference"}},
		"https://images.test/last.png",
	}, request.Body["images"], "the uploaded first frame must precede the last frame")

	ctx["files"] = []any{}
	ctx["requestBody"] = map[string]any{
		"prompt": "waves", "duration": 5,
		"images":          []any{"https://images.test/first.png", "data:" + "image/png;base64,aW1hZ2U="},
		"input_reference": "https://images.test/first.png",
	}
	require.NoError(t, plugin.Engine.CallInto(t.Context(), &request, "buildSubmitRequest", ctx))
	assert.Equal(t, []any{
		"https://images.test/first.png",
		map[string]any{"__videoUpload": map[string]any{"dataUrl": "data:" + "image/png;base64,aW1hZ2U="}},
	}, request.Body["images"])
	assert.Equal(t, "720P", request.Body["resolution"])
	assert.Equal(t, "16:9", request.Body["aspect_ratio"])
	assert.Equal(t, float64(5), request.Body["duration"])

	ctx["requestBody"] = map[string]any{"prompt": "waves", "input_reference": map[string]any{"file_id": "file-42"}}
	_, err = plugin.Engine.Call(t.Context(), "buildSubmitRequest", ctx)
	require.ErrorContains(t, err, "file IDs are not supported")

	ctx["videoRequestFormat"] = ""
	ctx["files"] = []any{map[string]any{"ref": "request_file:input_reference", "field": "input_reference"}}
	require.NoError(t, plugin.Engine.CallInto(t.Context(), &request, "buildSubmitRequest", ctx))
	assert.Equal(t, "multipart", request.BodyType, "unconfigured channels retain standard file submission")
}

func TestSoraCompatibleVideoBillingFacts(t *testing.T) {
	source, err := builtinplugins.Source("sora")
	require.NoError(t, err)
	plugin, err := jsplugin.NewRegistry().RegisterFactory(source, jsplugin.Options{Key: "sora"})
	require.NoError(t, err)
	call := func(hook string, args ...any) map[string]any {
		t.Helper()
		value, callErr := plugin.Engine.Call(t.Context(), hook, args...)
		require.NoError(t, callErr)
		encoded, marshalErr := common.Marshal(value)
		require.NoError(t, marshalErr)
		var result map[string]any
		require.NoError(t, common.Unmarshal(encoded, &result))
		return result
	}
	for _, model := range []string{"minimax_h3", "grok-imagine-video-1.5", "unknown-video-alias"} {
		t.Run(model, func(t *testing.T) {
			ctx := map[string]any{
				"model": model, "upstreamModel": model, "baseUrl": "https://video.test", "apiKey": "test-only",
				"requestBody": map[string]any{"prompt": "waves", "seconds": "6", "metadata": map[string]any{"resolution": "480p", "generate_audio": false}},
			}
			facts := call("extractUsage", ctx)
			assert.Equal(t, "480p", facts["resolution"])
			assert.Equal(t, float64(6), facts["seconds"])
			request := call("buildSubmitRequest", ctx)
			assert.Equal(t, "https://video.test/v1/videos", request["url"])
			body := request["body"].(map[string]any)
			assert.Equal(t, "480p", body["metadata"].(map[string]any)["resolution"])
			if model == "grok-imagine-video-1.5" {
				assert.Equal(t, "480p", body["resolution"])
				assert.Equal(t, false, body["generate_audio"])
			} else {
				assert.NotContains(t, body, "resolution")
			}
		})
	}
	for _, seconds := range []any{-1, 0, 3601, "NaN"} {
		_, err := plugin.Engine.Call(t.Context(), "extractUsage", map[string]any{
			"requestBody": map[string]any{"seconds": seconds},
		})
		assert.Error(t, err)
	}
	missing := call("extractUsage", map[string]any{"requestBody": map[string]any{"seconds": 6, "size": "1792x1024", "metadata": map[string]any{"resolution": "auto"}}})
	assert.NotContains(t, missing, "resolution", "dimensions and auto must not become an invented resolution")
	measured := call("extractUsageOnComplete", nil, nil, map[string]any{"seconds": 5, "resolution": "720p"})
	assert.Equal(t, float64(5), measured["seconds"])
	assert.Equal(t, "720p", measured["resolution"])
	for _, tc := range []struct {
		name, body, resolution string
		seconds                float64
	}{
		{"actual dimensions override label", `{"seconds":6,"size":"1920x1080","resolution":"720p"}`, "1080p", 6},
		{"portrait dimensions", `{"duration":5,"width":1080,"height":1920}`, "1080p", 5},
		{"nested output", `{"video":{"duration_seconds":4.5,"width":854,"height":480}}`, "480p", 4.5},
		{"resolution name", `{"seconds":6,"resolution_name":"720P"}`, "720p", 6},
		{"below 480 uses lowest tier", `{"size":"640x360"}`, "480p", 0},
		{"480 boundary uses lowest tier", `{"size":"854x480"}`, "480p", 0},
		{"above 480 uses middle tier", `{"width":854,"height":481}`, "720p", 0},
		{"720 boundary uses middle tier", `{"size":"1280x720"}`, "720p", 0},
		{"nonstandard portrait uses middle tier", `{"size":"768:1366"}`, "720p", 0},
		{"1024 short edge uses middle tier", `{"size":"1792x1024"}`, "720p", 0},
		{"below 1080 uses middle tier", `{"size":"1920x1079"}`, "720p", 0},
		{"above 1080 uses highest tier", `{"size":"3840x2160"}`, "1080p", 0},
		{"zero dimensions do not invent resolution", `{"width":0,"height":480}`, "", 0},
		{"negative dimensions do not invent resolution", `{"width":-1,"height":480}`, "", 0},
		{"oversized dimensions do not invent resolution", `{"width":65536,"height":480}`, "", 0},
		{"invalid duration is not billed", `{"seconds":3601}`, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]any
			require.NoError(t, common.UnmarshalJsonStr(tc.body, &body))
			actual := call("extractUsageOnComplete", nil, nil, body)
			if tc.resolution == "" {
				assert.NotContains(t, actual, "resolution")
			} else {
				assert.Equal(t, tc.resolution, actual["resolution"])
			}
			if tc.seconds == 0 {
				assert.NotContains(t, actual, "seconds")
			} else {
				assert.Equal(t, tc.seconds, actual["seconds"])
			}
		})
	}
	assert.Empty(t, call("extractUsageOnComplete", nil, nil, map[string]any{}))
}
