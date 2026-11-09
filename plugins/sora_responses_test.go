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
		{"unknown dimensions are not guessed", `{"size":"1792x1024"}`, "", 0},
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
