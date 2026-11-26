package jsplugin

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/plugins"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/abema/go-mp4"
	"github.com/gin-gonic/gin"
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
		require.NoError(t, plugin.Engine.CallInto(t.Context(), &submitted, "parseSubmitResponse", ctx, map[string]any{
			"status": 202, "body": map[string]any{"id": "upstream-123", "status_url": "https://private.example"},
		}))
		assert.Equal(t, map[string]any{"taskId": "upstream-123", "state": map[string]any{"reference_video_seconds": float64(0)}}, submitted)
		_, err = plugin.Engine.Call(t.Context(), "parseSubmitResponse", ctx, map[string]any{"body": map[string]any{}})
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
			facts, err := adaptor.ExtractUsageFactsValidated(c, info)
			require.NoError(t, err)
			assert.Equal(t, map[string]any{"seconds": float64(6)}, facts, "image references do not add video seconds")
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
				facts, err := adaptor.ExtractUsageFactsValidated(c, info)
				require.NoError(t, err)
				assert.Equal(t, map[string]any{"seconds": float64(6)}, facts, "audio references do not add video seconds")
			} else {
				require.ErrorContains(t, err, "audio tracks without video")
				assert.Zero(t, uploads, "a video renamed to M4A must not be uploaded as audio")
			}
		})
	}
}

func TestCCompatibleVideoUpscaling(t *testing.T) {
	service.InitHttpClient()
	source, err := plugins.Source("c-compatible")
	require.NoError(t, err)
	plugin, err := pluginruntime.NewRegistry().RegisterFactory(source, pluginruntime.Options{Key: "c-compatible"})
	require.NoError(t, err)
	for _, input := range []string{"multipart", "base64", "optional prompt", "short movie header", "short movie and track headers", "short media header", "long media track", "long prompt", "no video", "audio only", "zero duration", "too long", "unsupported format", "multiple videos", "wrong mapped model"} {
		t.Run(input, func(t *testing.T) {
			duration, handler := uint32(6500), [4]byte{'v', 'i', 'd', 'e'}
			if input == "zero duration" {
				duration = 0
			} else if input == "too long" {
				duration = 3600001
			} else if input == "audio only" {
				handler = [4]byte{'s', 'o', 'u', 'n'}
			}
			movieDuration, trackDuration, mediaDuration := duration, duration, duration
			if input == "short movie header" || input == "short movie and track headers" || input == "long media track" {
				movieDuration = 1000
			}
			if input == "short movie and track headers" || input == "long media track" {
				trackDuration = 1000
			}
			if input == "short media header" {
				mediaDuration = 1000
			} else if input == "long media track" {
				mediaDuration = 3600001
			}
			video := cCompatibleUpscaleMP4(t, movieDuration, trackDuration, mediaDuration, handler)
			if input == "unsupported format" {
				video = []byte("not a readable MP4 video")
			}
			uploads := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				uploads++
				assert.Equal(t, "/v1/media/uploads", r.URL.Path)
				assert.Equal(t, "Bearer secret", r.Header.Get("Authorization"))
				require.NoError(t, r.ParseMultipartForm(1<<20))
				file, _, err := r.FormFile("file")
				require.NoError(t, err)
				defer file.Close()
				data, err := io.ReadAll(file)
				require.NoError(t, err)
				assert.Equal(t, video, data)
				_, _ = w.Write([]byte(`{"media_id":"med_source_video"}`))
			}))
			defer upstream.Close()
			request := map[string]any{"seconds": 1, "resolution": "1080p", "metadata": map[string]any{"operation": "upscale"}}
			if input == "optional prompt" {
				request["prompt"] = "  Preserve faces and fine details\n"
			} else if input == "long prompt" {
				request["prompt"] = strings.Repeat("中", 8001)
			}
			c := newMultipartFileContext(t, "input_videos", "source.mp4", "video/mp4", video)
			if input == "base64" || input == "no video" || input == "multiple videos" {
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader("{}"))
				c.Request.Header.Set("Content-Type", "application/json")
				if input != "no video" {
					dataURL := "data:video/mp4;base64," + base64.StdEncoding.EncodeToString(video)
					request["input_videos"] = []any{dataURL}
					if input == "multiple videos" {
						request["input_videos"] = []any{dataURL, "data:video/mp4;base64," + base64.StdEncoding.EncodeToString(append(video, 0))}
					}
				}
			}
			c.Set("task_request", request)
			mapped := "DMC 视频超分"
			if input == "wrong mapped model" {
				mapped = "video-generation"
			}
			info := &relaycommon.RelayInfo{
				ChannelMeta:   &relaycommon.ChannelMeta{ChannelBaseUrl: upstream.URL, ApiKey: "secret", UpstreamModelName: mapped},
				TaskRelayInfo: &relaycommon.TaskRelayInfo{PublicTaskID: "task-public-upscale"}, OriginModelName: "client-upscale-alias",
			}
			adaptor := New(plugin)
			adaptor.Init(info)
			taskErr := adaptor.ValidateRequestAndSetAction(c, info)
			assert.Zero(t, uploads, "source validation and quota estimation must precede uploads")
			if input != "multipart" && input != "base64" && input != "optional prompt" && input != "short movie header" && input != "short movie and track headers" && input != "short media header" {
				require.NotNil(t, taskErr)
				return
			}
			require.Nil(t, taskErr)
			facts, err := adaptor.ExtractUsageFactsValidated(c, info)
			require.NoError(t, err)
			assert.Equal(t, float64(7), facts["seconds"], "client-supplied one-second reservation must be replaced")
			assert.Equal(t, 1, request["seconds"], "the original hook input must not be mutated")
			assert.Zero(t, uploads)
			body, err := adaptor.BuildRequestBody(c, info)
			require.NoError(t, err)
			var sent map[string]any
			require.NoError(t, common.DecodeJson(body, &sent))
			want := map[string]any{"model": "DMC 视频超分", "resolution": "1080p", "frame_rate": float64(0),
				"input_videos": []any{map[string]any{"media_id": "med_source_video"}}}
			if input == "optional prompt" {
				want["prompt"] = request["prompt"]
			}
			assert.Equal(t, want, sent)
			assert.Equal(t, 1, uploads)
			parsed, taskErr := adaptor.ParseResponse(c, &http.Response{StatusCode: http.StatusAccepted,
				Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":"upstream-upscale-task"}`))}, info)
			require.Nil(t, taskErr)
			require.NotNil(t, parsed)
			assert.JSONEq(t, `{"reference_video_seconds":0}`, string(parsed.PluginState), "upscaling must not freeze an additional source charge")
			var settled map[string]any
			require.NoError(t, plugin.Engine.CallInto(t.Context(), &settled, "extractUsageOnComplete", map[string]any{
				"model": "client-upscale-alias", "upstreamModel": "DMC 视频超分", "state": map[string]any{"reference_video_seconds": 0},
			}, nil, map[string]any{"seconds": 6.5}))
			assert.Equal(t, map[string]any{"seconds": 6.5}, settled)
		})
	}
	t.Run("upscaling aliases may decode without generation prompt", func(t *testing.T) {
		var intent map[string]any
		require.NoError(t, plugin.Engine.CallPathInto(t.Context(), &intent, "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
			"model": "client-upscale-alias", "body": map[string]any{"kind": "json", "value": map[string]any{
				"resolution": "1080p", "metadata": map[string]any{"operation": "upscale"}, "input_videos": []any{"data:video/mp4;base64,AA=="},
			}},
		}))
		assert.Equal(t, "upscale", intent["action"])
		_, err := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
			"model": "ordinary-video", "body": map[string]any{"kind": "json", "value": map[string]any{"seconds": 6}},
		})
		require.ErrorContains(t, err, "prompt is required")
	})
}

func TestCCompatibleVideoRemoval(t *testing.T) {
	service.InitHttpClient()
	source, err := plugins.Source("c-compatible")
	require.NoError(t, err)
	plugin, err := pluginruntime.NewRegistry().RegisterFactory(source, pluginruntime.Options{Key: "c-compatible"})
	require.NoError(t, err)
	t.Run("host rejects source above 64 MiB before reading", func(t *testing.T) {
		form := &multipart.Form{File: map[string][]*multipart.FileHeader{
			"input_videos": {{Size: 64<<20 + 1}},
		}}
		_, err := measureVideoSource(map[string]any{"fileRef": pluginruntime.FileReference("input_videos", 0)}, form)
		require.ErrorContains(t, err, "upload size limit")
	})
	for _, processing := range []struct{ model, operation string }{
		{"AI去字幕", "remove-subtitles"}, {"AI去水印", "remove-watermark"},
	} {
		for _, input := range []string{"base64", "multipart", "canonical model", "optional prompt", "no source", "multiple sources", "image", "audio", "long prompt", "invalid video", "too long", "wrong mapped model", "wrong operation", "opposite removal operation"} {
			t.Run(processing.model+"/"+input, func(t *testing.T) {
				video := cCompatibleUpscaleMP4(t, 6500, 6500, 6500, [4]byte{'v', 'i', 'd', 'e'})
				if input == "invalid video" {
					video = []byte("invalid MP4")
				} else if input == "too long" {
					video = cCompatibleUpscaleMP4(t, 3600001, 3600001, 3600001, [4]byte{'v', 'i', 'd', 'e'})
				}
				uploads := 0
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					uploads++
					assert.Equal(t, "/v1/media/uploads", r.URL.Path)
					_, _ = w.Write([]byte(`{"media_id":"med_removal_source"}`))
				}))
				defer upstream.Close()
				request := map[string]any{"seconds": 1, "__c_reference_video_seconds": 1,
					"metadata": map[string]any{"operation": processing.operation, "duration_seconds": 1,
						"size": "1280x720", "resolution": "720p", "aspect_ratio": "16:9", "frame_rate": 30,
						"video_reference_frame_mode": "first_frame", "generate_audio": false}}
				if input == "optional prompt" {
					request["prompt"] = "保留背景"
				} else if input == "long prompt" {
					request["prompt"] = strings.Repeat("中", 8001)
				} else {
					request["prompt"] = ""
				}
				dataURL := "data:video/mp4;base64," + base64.StdEncoding.EncodeToString(video)
				request["input_videos"] = []any{dataURL}
				if input == "no source" {
					delete(request, "input_videos")
				} else if input == "multiple sources" {
					request["input_videos"] = []any{dataURL, "data:video/mp4;base64," + base64.StdEncoding.EncodeToString(append(video, 0))}
				} else if input == "image" {
					request["input_images"] = []any{"data:image/png;base64,AA=="}
				} else if input == "audio" {
					request["input_audios"] = []any{"data:audio/wav;base64,AA=="}
				} else if input == "wrong operation" {
					request["metadata"].(map[string]any)["operation"] = "upscale"
				} else if input == "opposite removal operation" {
					operation := "remove-subtitles"
					if processing.operation == operation {
						operation = "remove-watermark"
					}
					request["metadata"].(map[string]any)["operation"] = operation
				}
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader("{}"))
				c.Request.Header.Set("Content-Type", "application/json")
				if input == "multipart" {
					delete(request, "input_videos")
					c = newMultipartFileContext(t, "input_videos", "source.mp4", "video/mp4", video)
				}
				mapped, origin := processing.model, "processing-alias"
				if input == "canonical model" {
					origin = mapped
					delete(request, "metadata")
				} else if input == "wrong mapped model" {
					mapped = "ordinary-video"
				}
				c.Set("task_request", request)
				info := &relaycommon.RelayInfo{
					ChannelMeta:   &relaycommon.ChannelMeta{ChannelBaseUrl: upstream.URL, ApiKey: "secret", UpstreamModelName: mapped},
					TaskRelayInfo: &relaycommon.TaskRelayInfo{PublicTaskID: "removal-video-task"}, OriginModelName: origin,
				}
				adaptor := New(plugin)
				adaptor.Init(info)
				taskErr := adaptor.ValidateRequestAndSetAction(c, info)
				assert.Zero(t, uploads, "validation and measurement must precede uploads")
				if input != "base64" && input != "multipart" && input != "canonical model" && input != "optional prompt" {
					require.NotNil(t, taskErr)
					return
				}
				require.Nil(t, taskErr)
				facts, err := adaptor.ExtractUsageFactsValidated(c, info)
				require.NoError(t, err)
				assert.Equal(t, map[string]any{"seconds": float64(7)}, facts)
				assert.Equal(t, 1, request["seconds"], "the client input stays immutable")
				ratios, err := adaptor.EstimateBillingValidated(c, info)
				require.NoError(t, err)
				assert.Equal(t, map[string]float64{"seconds": 7}, ratios)
				assert.Zero(t, uploads)
				body, err := adaptor.BuildRequestBody(c, info)
				require.NoError(t, err)
				var sent map[string]any
				require.NoError(t, common.DecodeJson(body, &sent))
				want := map[string]any{"model": mapped, "input_videos": []any{map[string]any{"media_id": "med_removal_source"}}}
				if input == "optional prompt" {
					want["prompt"] = request["prompt"]
				}
				assert.Equal(t, want, sent, "source processing strips all generation controls and private billing inputs")
				assert.Equal(t, 1, uploads)
				parsed, taskErr := adaptor.ParseResponse(c, &http.Response{StatusCode: http.StatusAccepted,
					Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":"upstream-removal-task"}`))}, info)
				require.Nil(t, taskErr)
				require.NotNil(t, parsed)
				assert.JSONEq(t, `{"source_video_seconds":7}`, string(parsed.PluginState))
				for _, actual := range []any{nil, 0, 9999, 3.5} {
					task := &model.Task{TaskID: "removal-video-task", Status: model.TaskStatusSuccess,
						Properties:  model.Properties{OriginModelName: origin, UpstreamModelName: mapped},
						PrivateData: model.TaskPrivateData{PluginState: parsed.PluginState}}
					encoded, err := common.Marshal(map[string]any{"status": "completed", "seconds": actual})
					require.NoError(t, err)
					result, err := adaptor.ParseTaskResult(task, &http.Response{StatusCode: http.StatusOK, Header: make(http.Header)}, encoded)
					require.NoError(t, err)
					assert.Equal(t, map[string]any{"seconds": float64(7)}, result.UsageFacts, "settlement bills the frozen source once regardless of output duration")
				}
			})
		}
		t.Run(processing.model+"/alias decode", func(t *testing.T) {
			var intent map[string]any
			require.NoError(t, plugin.Engine.CallPathInto(t.Context(), &intent, "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
				"model": "processing-alias", "body": map[string]any{"kind": "json", "value": map[string]any{
					"metadata": map[string]any{"operation": processing.operation}, "input_videos": []any{"data:video/mp4;base64,AA=="},
				}},
			}))
			assert.Equal(t, processing.operation, intent["action"])
		})
		t.Run(processing.model+"/invalid frozen source", func(t *testing.T) {
			for _, seconds := range []any{nil, 0, -1, 3601, 6.5, "7"} {
				_, err := plugin.Engine.Call(t.Context(), "extractUsageOnComplete", map[string]any{
					"model": "processing-alias", "upstreamModel": processing.model,
					"state": map[string]any{"source_video_seconds": seconds},
				}, nil, map[string]any{"seconds": 1})
				require.Error(t, err, "invalid frozen source must not become an upstream-based discount")
			}
		})
	}
}

func TestCCompatibleReferenceVideoBilling(t *testing.T) {
	service.InitHttpClient()
	source, err := plugins.Source("c-compatible")
	require.NoError(t, err)
	plugin, err := pluginruntime.NewRegistry().RegisterFactory(source, pluginruntime.Options{Key: "c-compatible"})
	require.NoError(t, err)
	for _, tc := range []struct {
		name         string
		milliseconds []uint32
		multipart    bool
		forged       bool
		metadata     bool
		want         float64
		invalid      bool
		invalidUsage bool
	}{
		{name: "multipart fractional source", milliseconds: []uint32{2250}, multipart: true, want: 8.25},
		{name: "multiple fractional data URLs", milliseconds: []uint32{1250, 2500}, want: 9.75},
		{name: "forged measured source", milliseconds: []uint32{2250}, forged: true, want: 8.25},
		{name: "forged source without video", forged: true, want: 6},
		{name: "final descriptor overrides metadata references", milliseconds: []uint32{1500}, metadata: true, want: 7.5},
		{name: "total duration at ceiling", milliseconds: []uint32{3594000}, want: 3600},
		{name: "total duration above ceiling", milliseconds: []uint32{3594500}, invalidUsage: true},
		{name: "invalid source metadata", milliseconds: []uint32{0}, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uploads := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				uploads++
				assert.Equal(t, "/v1/media/uploads", r.URL.Path)
				_, _ = w.Write([]byte(`{"media_id":"med_reference"}`))
			}))
			defer upstream.Close()
			request := map[string]any{"prompt": "waves", "seconds": 6}
			if tc.forged {
				request["__c_reference_video_seconds"] = 9999
			}
			if tc.metadata {
				ignored := cCompatibleUpscaleMP4(t, 3600000, 3600000, 3600000, [4]byte{'v', 'i', 'd', 'e'})
				request["metadata"] = map[string]any{"input_videos": []any{"data:video/mp4;base64," + base64.StdEncoding.EncodeToString(ignored)}}
			}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader("{}"))
			c.Request.Header.Set("Content-Type", "application/json")
			videos := make([]any, 0, len(tc.milliseconds))
			for _, milliseconds := range tc.milliseconds {
				video := cCompatibleUpscaleMP4(t, milliseconds, milliseconds, milliseconds, [4]byte{'v', 'i', 'd', 'e'})
				if tc.multipart {
					c = newMultipartFileContext(t, "input_videos", "reference.mp4", "video/mp4", video)
				} else {
					videos = append(videos, "data:video/mp4;base64,"+base64.StdEncoding.EncodeToString(video))
				}
			}
			if len(videos) > 0 {
				request["input_videos"] = videos
			}
			c.Set("task_request", request)
			info := &relaycommon.RelayInfo{
				ChannelMeta:   &relaycommon.ChannelMeta{ChannelBaseUrl: upstream.URL, ApiKey: "secret", UpstreamModelName: "mapped-video-model"},
				TaskRelayInfo: &relaycommon.TaskRelayInfo{PublicTaskID: "reference-video-task"}, OriginModelName: "client-alias",
			}
			adaptor := New(plugin)
			adaptor.Init(info)
			taskErr := adaptor.ValidateRequestAndSetAction(c, info)
			assert.Zero(t, uploads, "reference measurement must not upload before quota reservation")
			if tc.invalid {
				require.NotNil(t, taskErr)
				return
			}
			require.Nil(t, taskErr)
			facts, err := adaptor.ExtractUsageFactsValidated(c, info)
			if tc.invalidUsage {
				require.Error(t, err)
				_, err = adaptor.EstimateBillingValidated(c, info)
				require.Error(t, err)
				assert.Zero(t, uploads, "an oversized combined quantity is rejected before reservation or upload")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, map[string]any{"seconds": tc.want}, facts)
			ratios, err := adaptor.EstimateBillingValidated(c, info)
			require.NoError(t, err)
			assert.Equal(t, map[string]float64{"seconds": tc.want}, ratios, "legacy pricing multiplies the combined seconds only once")
			assert.Zero(t, uploads)
			body, err := adaptor.BuildRequestBody(c, info)
			require.NoError(t, err)
			var sent map[string]any
			require.NoError(t, common.DecodeJson(body, &sent))
			assert.Equal(t, float64(6), sent["seconds"], "upstream duration remains output duration")
			assert.NotContains(t, sent, "__c_reference_video_seconds")
			assert.Equal(t, len(tc.milliseconds), uploads)
			assert.Equal(t, 6, request["seconds"])
			if tc.forged {
				assert.Equal(t, 9999, request["__c_reference_video_seconds"], "host measurement does not mutate client input")
			}
			parsed, taskErr := adaptor.ParseResponse(c, &http.Response{StatusCode: http.StatusAccepted,
				Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":"upstream-reference-task"}`))}, info)
			require.Nil(t, taskErr)
			require.NotNil(t, parsed)
			var state map[string]any
			require.NoError(t, common.Unmarshal(parsed.PluginState, &state))
			assert.Equal(t, map[string]any{"reference_video_seconds": tc.want - 6}, state)
			task := &model.Task{TaskID: "reference-video-task", Status: model.TaskStatusSuccess,
				Properties:  model.Properties{OriginModelName: "client-alias", UpstreamModelName: "mapped-video-model"},
				PrivateData: model.TaskPrivateData{PluginState: parsed.PluginState}}
			result, err := adaptor.ParseTaskResult(task, &http.Response{StatusCode: http.StatusOK, Header: make(http.Header)},
				[]byte(`{"status":"completed","seconds":5.5,"reference_video_seconds":9999}`))
			require.NoError(t, err)
			assert.Equal(t, map[string]any{"seconds": tc.want - 0.5}, result.UsageFacts, "completion retains frozen source seconds")
			adaptor.AdjustBillingOnComplete(task, result)
			assert.Equal(t, map[string]any{"seconds": tc.want - 0.5}, result.UsageFacts)
		})
	}
}

func TestCCompatibleReferenceVideoCompletionBilling(t *testing.T) {
	source, err := plugins.Source("c-compatible")
	require.NoError(t, err)
	plugin, err := pluginruntime.NewRegistry().RegisterFactory(source, pluginruntime.Options{Key: "c-compatible"})
	require.NoError(t, err)
	for _, tc := range []struct {
		name   string
		state  string
		actual any
		want   float64
	}{
		{name: "frozen source plus actual output", state: `{"reference_video_seconds":3}`, actual: 5, want: 8},
		{name: "fractional source plus fractional output", state: `{"reference_video_seconds":3.75}`, actual: 5.5, want: 9.25},
		{name: "no source", state: `{"reference_video_seconds":0}`, actual: 5, want: 5},
		{name: "legacy task", actual: 5.5, want: 5.5},
		{name: "missing actual", state: `{"reference_video_seconds":3}`},
		{name: "zero actual", state: `{"reference_video_seconds":3}`, actual: 0},
		{name: "negative actual", state: `{"reference_video_seconds":3}`, actual: -1},
		{name: "invalid actual", state: `{"reference_video_seconds":3}`, actual: true},
		{name: "nonfinite actual", state: `{"reference_video_seconds":3}`, actual: "NaN"},
		{name: "actual above ceiling", state: `{"reference_video_seconds":3}`, actual: 3601},
		{name: "combined actual above ceiling", state: `{"reference_video_seconds":3}`, actual: 3600},
		{name: "missing frozen quantity", state: `{}`, actual: 5},
		{name: "negative frozen quantity", state: `{"reference_video_seconds":-1}`, actual: 5},
		{name: "invalid frozen quantity", state: `{"reference_video_seconds":"3"}`, actual: 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adaptor := New(plugin)
			adaptor.Init(&relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelBaseUrl: "https://upstream.example"}})
			snapshot := &billingexpr.BillingSnapshot{ExprString: `tier("base", u("seconds") * 0.1)`,
				TaskUsageBilling: true, ExprVersion: 1, QuotaPerUnit: 1, GroupRatio: 1,
				UsageFacts: map[string]any{"seconds": float64(9)}, VideoUsagePending: false}
			task := &model.Task{TaskID: "completion-video-task", Status: model.TaskStatusSuccess,
				Properties:  model.Properties{OriginModelName: "client-alias", UpstreamModelName: "mapped-video-model"},
				PrivateData: model.TaskPrivateData{PluginState: []byte(tc.state), BillingContext: &model.TaskBillingContext{TieredSnapshot: snapshot}}}
			body := map[string]any{"status": "completed", "reference_video_seconds": 9999}
			if tc.actual != nil {
				body["seconds"] = tc.actual
			}
			encoded, err := common.Marshal(body)
			require.NoError(t, err)
			result, err := adaptor.ParseTaskResult(task, &http.Response{StatusCode: http.StatusOK, Header: make(http.Header)}, encoded)
			require.NoError(t, err)
			assert.Equal(t, model.TaskStatusSuccess, result.Status)
			if tc.want == 0 {
				assert.Empty(t, result.UsageFacts, "missing or invalid completion facts preserve the full reservation")
			} else {
				assert.Equal(t, map[string]any{"seconds": tc.want}, result.UsageFacts)
			}
			adaptor.AdjustBillingOnComplete(task, result)
			settlement, facts, err := service.EvaluateTaskCompletionUsage(snapshot, result.UsageFacts)
			require.NoError(t, err)
			want := tc.want
			if want == 0 {
				want = 9
			}
			assert.Equal(t, map[string]any{"seconds": want}, facts)
			assert.InDelta(t, want*0.1, settlement.ActualQuotaBeforeGroup, 1e-12, "the frozen per-second unit price is unchanged")
			assert.Equal(t, map[string]any{"seconds": float64(9)}, snapshot.UsageFacts)
			assert.Equal(t, tc.state, string(task.PrivateData.PluginState))
		})
	}
}

func cCompatibleUpscaleMP4(t *testing.T, movieDuration, trackDuration, mediaDuration uint32, handler [4]byte) []byte {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "source-*.mp4")
	require.NoError(t, err)
	writer := mp4.NewWriter(file)
	for _, header := range []mp4.IImmutableBox{&mp4.Ftyp{MajorBrand: [4]byte{'m', 'p', '4', '2'}}, &mp4.Mvhd{Timescale: 1000, DurationV0: movieDuration},
		&mp4.Tkhd{TrackID: 1, DurationV0: trackDuration, Width: 1920 << 16, Height: 1080 << 16}, &mp4.Mdhd{Timescale: 1000, DurationV0: mediaDuration}, &mp4.Hdlr{HandlerType: handler}} {
		if header.GetType() == mp4.BoxTypeMvhd() {
			_, err = writer.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMoov()})
			require.NoError(t, err)
		} else if header.GetType() == mp4.BoxTypeTkhd() {
			_, err = writer.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeTrak()})
			require.NoError(t, err)
		} else if header.GetType() == mp4.BoxTypeMdhd() {
			_, err = writer.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdia()})
			require.NoError(t, err)
		}
		_, err = writer.StartBox(&mp4.BoxInfo{Type: header.GetType()})
		require.NoError(t, err)
		_, err = mp4.Marshal(writer, header, mp4.Context{})
		require.NoError(t, err)
		_, err = writer.EndBox()
		require.NoError(t, err)
	}
	for range 3 {
		_, err = writer.EndBox()
		require.NoError(t, err)
	}
	require.NoError(t, file.Close())
	data, err := os.ReadFile(file.Name())
	require.NoError(t, err)
	return data
}
