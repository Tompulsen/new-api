package jsplugin

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/plugins"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/abema/go-mp4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCCompatibleVideoContract(t *testing.T) {
	source, err := plugins.Source("c-compatible")
	require.NoError(t, err)
	plugin, err := pluginruntime.NewRegistry().RegisterFactory(source, pluginruntime.Options{Key: "c-compatible"})
	require.NoError(t, err)
	assert.Equal(t, "C兼容", plugin.Meta.Name)
	assert.Empty(t, plugin.Meta.Models)
	ctx := map[string]any{
		"baseUrl": "https://upstream.example/v1/", "apiKey": "secret",
		"publicTaskId": "task-public-123", "model": "client-alias", "upstreamModel": "another-video-model",
		"requestBody": map[string]any{
			"prompt": "waves", "seconds": "6", "size": "1368x768",
			"metadata": map[string]any{"duration": 10, "resolution": "768p", "aspect_ratio": "16:9", "unrelated_field": "ignored"},
		},
	}
	var descriptor requestDescriptor
	require.NoError(t, plugin.Engine.CallInto(t.Context(), &descriptor, "buildSubmitRequest", ctx))
	assert.Equal(t, "https://upstream.example/v1/videos", descriptor.URL)
	assert.Equal(t, "task-public-123", descriptor.Headers["Idempotency-Key"])
	assert.Equal(t, map[string]any{
		"model": "another-video-model", "prompt": "waves", "seconds": float64(6),
		"size": "1368x768", "resolution": "768p", "aspect_ratio": "16:9",
	}, descriptor.Body)
	var facts map[string]any
	require.NoError(t, plugin.Engine.CallInto(t.Context(), &facts, "extractUsage", ctx))
	assert.Equal(t, map[string]any{"seconds": float64(6)}, facts)
	for _, req := range []map[string]any{
		{}, {"seconds": 0}, {"seconds": -1}, {"seconds": 3601}, {"seconds": 1.5}, {"seconds": true},
		{"metadata": map[string]any{"duration": 1e30}}, {"seconds": 6, "metadata": "invalid"},
		{"seconds": 6, "input_images": []any{map[string]any{"media_id": "med_other-user"}}},
		{"seconds": 6, "images": []any{"https://private.example/ref.png"}},
		{"seconds": 6, "images": []any{map[string]any{"__videoUpload": map[string]any{"dataUrl": "secret"}}}},
		{"seconds": 6, "generate_audio": true}, {"seconds": 6, "frame_rate": -1},
		{"seconds": 6, "generation_options": map[string]any{"seconds": 9999}},
	} {
		req["prompt"] = "waves"
		ctx["requestBody"] = req
		_, err := plugin.Engine.Call(t.Context(), "buildSubmitRequest", ctx)
		require.Error(t, err, "%v", req)
	}
	t.Run("other models preserve explicit specifications and false values", func(t *testing.T) {
		ctx["upstreamModel"] = "second-video-model"
		ctx["requestBody"] = map[string]any{"prompt": "waves", "duration": 12, "resolution": "4K", "generate_audio": false, "frame_rate": 0}
		var request requestDescriptor
		require.NoError(t, plugin.Engine.CallInto(t.Context(), &request, "buildSubmitRequest", ctx))
		assert.Equal(t, map[string]any{"model": "second-video-model", "prompt": "waves", "seconds": float64(12), "resolution": "4K", "generate_audio": false, "frame_rate": float64(0)}, request.Body)
	})
	t.Run("multipart aliases decode with the same usage", func(t *testing.T) {
		var intent map[string]any
		require.NoError(t, plugin.Engine.CallPathInto(t.Context(), &intent, "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
			"model": "client-alias", "body": map[string]any{"kind": "multipart", "fields": map[string]any{
				"prompt": []any{"waves"}, "seconds": []any{"6"}, "metadata": []any{`{"duration":10,"resolution":"768p"}`},
			}},
		}))
		assert.Equal(t, "client-alias", intent["model"])
		assert.Equal(t, "text_to_video", intent["action"])
		ctx["requestBody"] = intent["requestBody"]
		require.NoError(t, plugin.Engine.CallInto(t.Context(), &facts, "extractUsage", ctx))
		assert.Equal(t, float64(6), facts["seconds"])
	})
	t.Run("private upstream data is never rendered", func(t *testing.T) {
		var rendered map[string]any
		require.NoError(t, plugin.Engine.CallPathInto(t.Context(), &rendered, "protocols", []string{"openai_video", "render"}, nil, map[string]any{
			"status": "SUCCESS", "data": map[string]any{"public_model_name": "private-provider", "content_url": "https://private.example", "error": "private-provider"},
		}))
		assert.Empty(t, rendered)
		var submitted map[string]any
		require.NoError(t, plugin.Engine.CallInto(t.Context(), &submitted, "parseSubmitResponse", nil, map[string]any{
			"status": 202, "body": map[string]any{"id": "upstream-123", "status_url": "https://private.example"},
		}))
		assert.Equal(t, map[string]any{"taskId": "upstream-123"}, submitted)
		_, err = plugin.Engine.Call(t.Context(), "parseSubmitResponse", nil, map[string]any{"body": map[string]any{}})
		require.ErrorContains(t, err, "task ID")
	})
	t.Run("lifecycle and actual usage", func(t *testing.T) {
		for upstream, want := range map[string]string{"queued": "QUEUED", "processing": "IN_PROGRESS", "completed": "SUCCESS", "failed": "FAILURE", "requires_attention": "UNKNOWN", "": "UNKNOWN"} {
			var result map[string]any
			require.NoError(t, plugin.Engine.CallInto(t.Context(), &result, "parseTaskResult", nil, map[string]any{"status": upstream, "error": "private-provider"}))
			assert.Equal(t, want, result["status"])
			if reason, ok := result["reason"].(string); ok {
				assert.NotContains(t, reason, "private-provider")
			}
		}
		for _, tc := range []struct {
			value any
			want  map[string]any
		}{{6.5, map[string]any{"seconds": 6.5}}, {0, map[string]any{}}, {-1, map[string]any{}}, {3601, map[string]any{}}, {true, map[string]any{}}} {
			var actual map[string]any
			require.NoError(t, plugin.Engine.CallInto(t.Context(), &actual, "extractUsageOnComplete", nil, nil, map[string]any{"duration_seconds": tc.value}))
			assert.Equal(t, tc.want, actual)
		}
	})
}

func TestCCompatibleMediaAndTaskRequests(t *testing.T) {
	service.InitHttpClient()
	source, err := plugins.Source("c-compatible")
	require.NoError(t, err)
	plugin, err := pluginruntime.NewRegistry().RegisterFactory(source, pluginruntime.Options{Key: "c-compatible"})
	require.NoError(t, err)
	var imageBytes bytes.Buffer
	require.NoError(t, png.Encode(&imageBytes, image.NewRGBA(image.Rect(0, 0, 2, 2))))
	for _, input := range []string{"multipart", "base64", "upload failure", "invalid media ID"} {
		t.Run(input, func(t *testing.T) {
			uploads, queries := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "Bearer secret", r.Header.Get("Authorization"))
				switch r.URL.Path {
				case "/v1/media/uploads":
					uploads++
					require.NoError(t, r.ParseMultipartForm(1<<20))
					assert.Empty(t, r.MultipartForm.Value, "upload must contain only the file")
					require.Len(t, r.MultipartForm.File, 1)
					file, _, err := r.FormFile("file")
					require.NoError(t, err)
					defer file.Close()
					data, err := io.ReadAll(file)
					require.NoError(t, err)
					assert.Equal(t, imageBytes.Bytes(), data)
					if input == "upload failure" {
						w.WriteHeader(http.StatusBadGateway)
						return
					}
					w.WriteHeader(http.StatusCreated)
					if input == "invalid media ID" {
						_, _ = w.Write([]byte(`{"media_id":"https://private.example"}`))
					} else {
						_, _ = w.Write([]byte(`{"media_id":"med_uploaded_reference"}`))
					}
				case "/v1/tasks/upstream-123":
					queries++
					_, _ = w.Write([]byte(`{"id":"upstream-123","status":"completed","seconds":6.5}`))
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			c := newMultipartFileContext(t, "input_reference", "ref.png", "image/png", imageBytes.Bytes())
			req := map[string]any{"prompt": "waves", "seconds": 6}
			if input != "multipart" {
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader("{}"))
				c.Request.Header.Set("Content-Type", "application/json")
				dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(imageBytes.Bytes())
				req["images"] = []any{dataURL, dataURL}
			}
			c.Set("task_request", req)
			info := &relaycommon.RelayInfo{
				ChannelMeta:   &relaycommon.ChannelMeta{ChannelBaseUrl: server.URL + "/v1", ApiKey: "secret", UpstreamModelName: "video-model"},
				TaskRelayInfo: &relaycommon.TaskRelayInfo{PublicTaskID: "task-public-123"}, OriginModelName: "client-alias",
			}
			adaptor := New(plugin)
			adaptor.Init(info)
			_, err = adaptor.buildSubmit(c, info)
			require.NoError(t, err)
			assert.Zero(t, uploads, "validation must not upload before billing reservation")
			reader, err := adaptor.BuildRequestBody(c, info)
			if input == "upload failure" || input == "invalid media ID" {
				require.Error(t, err)
				assert.Equal(t, 1, uploads)
				return
			}
			require.NoError(t, err)
			var body map[string]any
			require.NoError(t, common.DecodeJson(reader, &body))
			assert.Equal(t, []any{map[string]any{"media_id": "med_uploaded_reference"}}, body["input_images"])
			assert.NotContains(t, body, "input_reference")
			assert.Equal(t, 1, uploads)
			task := &model.Task{TaskID: "task-public-123", Status: model.TaskStatusSuccess}
			task.PrivateData.UpstreamTaskID = "upstream-123"
			response, err := adaptor.FetchTask(server.URL+"/v1", "secret", task, "")
			require.NoError(t, err)
			reply, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			assert.Equal(t, 1, queries)
			result, err := adaptor.ParseTaskResult(task, response, reply)
			require.NoError(t, err)
			assert.Equal(t, model.TaskStatusSuccess, result.Status)
			assert.Equal(t, float64(6.5), result.UsageFacts["seconds"])
			content, err := adaptor.BuildContentRequest(t.Context(), task, "video", channel.TaskArtifactClientRequest{Method: http.MethodHead})
			require.NoError(t, err)
			assert.Equal(t, server.URL+"/v1/tasks/upstream-123/content?delivery=stream", content.URL)
			assert.Equal(t, http.MethodHead, content.Method)
			assert.True(t, content.StripCredentialsOnRedirect)
		})
	}
}

func TestCCompatibleMP4AudioRequiresAudioOnlyTracks(t *testing.T) {
	service.InitHttpClient()
	source, err := plugins.Source("c-compatible")
	require.NoError(t, err)
	plugin, err := pluginruntime.NewRegistry().RegisterFactory(source, pluginruntime.Options{Key: "c-compatible"})
	require.NoError(t, err)
	for _, tracks := range []string{"soun", "vide", "sounvide"} {
		t.Run(tracks, func(t *testing.T) {
			file, err := os.CreateTemp(t.TempDir(), "reference-*.m4a")
			require.NoError(t, err)
			writer := mp4.NewWriter(file)
			_, err = writer.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeFtyp()})
			require.NoError(t, err)
			_, err = mp4.Marshal(writer, &mp4.Ftyp{MajorBrand: [4]byte{'M', '4', 'A', ' '}}, mp4.Context{})
			require.NoError(t, err)
			_, err = writer.EndBox()
			require.NoError(t, err)
			_, err = writer.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMoov()})
			require.NoError(t, err)
			for index := 0; index < len(tracks); index += 4 {
				for _, kind := range []mp4.BoxType{mp4.BoxTypeTrak(), mp4.BoxTypeMdia(), mp4.BoxTypeHdlr()} {
					_, err = writer.StartBox(&mp4.BoxInfo{Type: kind})
					require.NoError(t, err)
				}
				var handler [4]byte
				copy(handler[:], tracks[index:index+4])
				_, err = mp4.Marshal(writer, &mp4.Hdlr{HandlerType: handler}, mp4.Context{})
				require.NoError(t, err)
				for range 3 {
					_, err = writer.EndBox()
					require.NoError(t, err)
				}
			}
			_, err = writer.EndBox()
			require.NoError(t, err)
			require.NoError(t, file.Close())
			data, err := os.ReadFile(file.Name())
			require.NoError(t, err)
			uploads := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				uploads++
				require.NoError(t, r.ParseMultipartForm(1<<20))
				require.Len(t, r.MultipartForm.File["file"], 1)
				assert.Equal(t, "audio/mp4", r.MultipartForm.File["file"][0].Header.Get("Content-Type"))
				_, _ = w.Write([]byte(`{"media_id":"med_audio"}`))
			}))
			defer server.Close()
			c := newMultipartFileContext(t, "reference_audios", "ref.m4a", "audio/mp4", data)
			c.Set("task_request", map[string]any{"prompt": "waves", "seconds": 6})
			info := &relaycommon.RelayInfo{
				ChannelMeta:   &relaycommon.ChannelMeta{ChannelBaseUrl: server.URL, ApiKey: "secret"},
				TaskRelayInfo: &relaycommon.TaskRelayInfo{PublicTaskID: "task-public-123"}, OriginModelName: "video-model",
			}
			adaptor := New(plugin)
			adaptor.Init(info)
			_, err = adaptor.BuildRequestBody(c, info)
			if tracks == "soun" {
				require.NoError(t, err)
				assert.Equal(t, 1, uploads)
			} else {
				require.ErrorContains(t, err, "audio tracks without video")
				assert.Zero(t, uploads, "a video renamed to M4A must not be uploaded as audio")
			}
		})
	}
}
