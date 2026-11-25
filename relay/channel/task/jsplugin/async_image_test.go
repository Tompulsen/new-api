package jsplugin

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/plugins"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAsyncImageConfigurableTaskContract(t *testing.T) {
	source, err := plugins.Source("async-image")
	require.NoError(t, err)
	plugin, err := pluginruntime.NewRegistry().RegisterFactory(source, pluginruntime.Options{Key: "async-image"})
	require.NoError(t, err)
	config := plugin.ChannelConfigFor(nil)
	assert.Contains(t, string(config), `"image_async_enabled":false`)
	var settings map[string]any
	require.NoError(t, common.Unmarshal(config, &settings))
	ctx := map[string]any{"config": settings, "baseUrl": "https://vendor.example/v1", "apiKey": "private-key", "publicTaskId": "task_public", "model": "alias", "upstreamModel": "gpt-image-2-4k", "requestBody": map[string]any{"prompt": "poster", "n": 1, "size": "2304x3456", "quality": "4K"}}
	var request requestDescriptor
	require.NoError(t, plugin.Engine.CallInto(t.Context(), &request, "buildSubmitRequest", ctx))
	assert.Equal(t, "https://vendor.example/v1/images/generations", request.URL)
	assert.Equal(t, "Bearer private-key", request.Headers["Authorization"])
	body := request.Body.(map[string]any)
	assert.Equal(t, "gpt-image-2-4k", body["model"])
	assert.Equal(t, "2304x3456", body["size"])
	assert.Equal(t, "url", body["response_format"])
	assert.NotContains(t, body, "wait", "the vendor's undocumented wait default must not be guessed")
	settings["submit_parameters"] = map[string]any{"response_format": "async"}
	require.NoError(t, plugin.Engine.CallInto(t.Context(), &request, "buildSubmitRequest", ctx))
	assert.Equal(t, "async", request.Body.(map[string]any)["response_format"])
	ctx["files"] = []any{map[string]any{"field": "image", "ref": "ref-1", "filename": "reference.png"}}
	require.NoError(t, plugin.Engine.CallInto(t.Context(), &request, "buildSubmitRequest", ctx))
	assert.Equal(t, "multipart", request.BodyType)
	assert.Contains(t, request.Parts, requestPart{Name: "response_format", Value: "async"})
	delete(ctx, "files")

	settings["submit_parameters"] = map[string]any{"wait": false, "n": 2}
	var facts map[string]any
	require.NoError(t, plugin.Engine.CallInto(t.Context(), &facts, "extractUsage", ctx))
	assert.Equal(t, float64(2), facts["image_count"])
	require.NoError(t, plugin.Engine.CallInto(t.Context(), &request, "buildSubmitRequest", ctx))
	assert.Equal(t, false, request.Body.(map[string]any)["wait"])
	for _, invalid := range []any{0, -1, 129, true, "", "1.5"} {
		settings["submit_parameters"] = map[string]any{"n": invalid}
		_, err := plugin.Engine.Call(t.Context(), "buildSubmitRequest", ctx)
		require.Error(t, err)
	}
	settings["submit_parameters"] = map[string]any{"wait": false}
	stored, err := plugin.ValidateChannelConfig([]byte(`{"submit_parameters":{"wait":false}}`))
	require.NoError(t, err)
	assert.Contains(t, string(stored), `"wait":false`)

	for _, unsafe := range []string{"https://other.example/submit", "//other.example/submit", "/\\other.example", "/v1/images#fragment"} {
		settings["submit_path"] = unsafe
		_, err := plugin.Engine.Call(t.Context(), "buildSubmitRequest", ctx)
		require.Error(t, err, unsafe)
	}
	settings["submit_path"] = "/v1/images/generations"

	var submitted map[string]any
	require.NoError(t, plugin.Engine.CallInto(t.Context(), &submitted, "parseSubmitResponse", ctx, map[string]any{"statusCode": 202, "body": map[string]any{"task_no": "vendor/id?private"}}))
	assert.Equal(t, "vendor/id?private", submitted["taskId"])
	assert.Equal(t, map[string]any{}, submitted["taskData"])
	ctx["state"] = submitted["state"]
	ctx["taskId"] = submitted["taskId"]
	ctx["config"] = map[string]any{"query_path": "/wrong/{task_id}"}
	require.NoError(t, plugin.Engine.CallInto(t.Context(), &request, "buildQueryRequest", ctx))
	assert.Equal(t, "https://vendor.example/v1/tasks/vendor%2Fid%3Fprivate", request.URL, "polling uses the submitted configuration snapshot")

	for _, tc := range []struct{ upstream, want string }{{"queued", "QUEUED"}, {"processing", "IN_PROGRESS"}, {"completed", "SUCCESS"}, {"failed", "FAILURE"}} {
		var result taskResult
		body := map[string]any{"status": tc.upstream, "progress": 42, "result": map[string]any{"data": []any{map[string]any{"url": "https://cdn.example/image.png"}}}, "error": map[string]any{"message": "generation rejected"}}
		require.NoError(t, plugin.Engine.CallInto(t.Context(), &result, "parseTaskResult", ctx, body))
		assert.Equal(t, tc.want, result.Status)
		assert.Equal(t, "42%", result.Progress)
		if tc.want == "FAILURE" {
			assert.Equal(t, "generation rejected", result.Reason)
		}
	}
	for _, body := range []any{map[string]any{"status": "completed"}, map[string]any{"status": "not-documented"}, map[string]any{"error": "temporary query failure"}} {
		_, err := plugin.Engine.Call(t.Context(), "parseTaskResult", ctx, body)
		require.Error(t, err, "an invalid query must remain retryable rather than become a terminal generation failure")
	}

	completed := map[string]any{"result": map[string]any{"data": []any{map[string]any{"url": "https://cdn.example/one.png"}, map[string]any{"url": "https://cdn.example/two.png"}}}}
	require.NoError(t, plugin.Engine.CallInto(t.Context(), &facts, "extractUsageOnComplete", ctx, nil, completed))
	assert.Equal(t, float64(2), facts["image_count"])
	artifactCtx := map[string]any{"status": "SUCCESS", "state": submitted["state"], "data": completed, "artifactKey": "image-2", "clientRequest": map[string]any{"method": "GET"}}
	var artifacts []map[string]any
	require.NoError(t, plugin.Engine.CallInto(t.Context(), &artifacts, "listArtifacts", artifactCtx))
	require.Len(t, artifacts, 2)
	request = requestDescriptor{}
	require.NoError(t, plugin.Engine.CallInto(t.Context(), &request, "buildContentRequest", artifactCtx))
	assert.Equal(t, "https://cdn.example/two.png", request.URL)
	assert.True(t, request.Credentialless)
	assert.Empty(t, request.Headers)
	var rendered map[string]any
	require.NoError(t, plugin.Engine.CallPathInto(t.Context(), &rendered, "protocols", []string{"openai_image", "render"}, map[string]any{"state": submitted["state"]}, map[string]any{"data": completed}))
	assert.Len(t, rendered["data"], 2)
}

func TestAsyncImageAdaptorSubmitsOnceAndRestoresPrivatePollingState(t *testing.T) {
	useCredentialTestDB(t, &model.Channel{})
	source, err := plugins.Source("async-image")
	require.NoError(t, err)
	plugin, err := pluginruntime.NewRegistry().RegisterFactory(source, pluginruntime.Options{Key: "async-image"})
	require.NoError(t, err)
	posts, queries := 0, 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer original-key", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			posts++
			assert.Equal(t, "/v1/images/generations", r.URL.Path)
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"task_no":"up-original"}`))
			return
		}
		queries++
		assert.Equal(t, "/v1/tasks/up-original", r.URL.Path)
		if queries == 1 {
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"error":"temporary"}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"completed","result":{"data":[{"url":"https://cdn.example/result.png"}]}}`))
	}))
	t.Cleanup(upstream.Close)
	stored := &model.Channel{Type: constant.ChannelTypeTaskPlugin, Key: "original-key", Status: common.ChannelStatusEnabled, Models: "gpt-image-2-4k", Group: "default", BaseURL: &upstream.URL}
	stored.SetSetting(dto.ChannelSettings{TaskPluginKey: "async-image"})
	stored.SetOtherSettings(dto.ChannelOtherSettings{PluginConfig: []byte(`{"image_async_enabled":true}`)})
	require.NoError(t, model.DB.Create(stored).Error)
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelType: stored.Type, ChannelId: stored.Id, ChannelBaseUrl: upstream.URL, ApiKey: "original-key", UpstreamModelName: "gpt-image-2-4k"}, TaskRelayInfo: &relaycommon.TaskRelayInfo{PublicTaskID: "task-public"}, OriginModelName: "gpt-image-2-4k"}
	adaptor := New(plugin)
	adaptor.Init(info)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{}`))
	c.Set("task_request", relaycommon.TaskSubmitReq{Prompt: "poster"})
	body, err := adaptor.BuildRequestBody(c, info)
	require.NoError(t, err)
	response, err := adaptor.DoRequest(c, info, body)
	require.NoError(t, err)
	submission, taskErr := adaptor.ParseResponse(c, response, info)
	response.Body.Close()
	require.Nil(t, taskErr)
	require.Equal(t, "up-original", submission.UpstreamTaskID)
	task := &model.Task{TaskID: "task-public", ChannelId: stored.Id, Platform: "async-image", Properties: model.Properties{OriginModelName: "gpt-image-2-4k", UpstreamModelName: "gpt-image-2-4k"}}
	task.PrivateData.UpstreamTaskID, task.PrivateData.PluginState, task.PrivateData.Key = submission.UpstreamTaskID, submission.PluginState, "original-key"
	// A restarted worker can resume from the task's persisted private fields.
	for round := range 2 {
		worker := New(plugin)
		worker.Init(info)
		query, err := worker.FetchTask(upstream.URL, "replacement-key", task, "")
		require.NoError(t, err)
		if round == 0 {
			assert.Equal(t, 503, query.StatusCode)
			query.Body.Close()
			continue
		}
		queryBody, err := io.ReadAll(query.Body)
		require.NoError(t, err)
		result, err := worker.ParseTaskResult(task, query, queryBody)
		query.Body.Close()
		require.NoError(t, err)
		assert.Equal(t, "SUCCESS", result.Status)
		assert.Equal(t, "https://cdn.example/result.png", result.Url)
	}
	assert.Equal(t, 1, posts)
	assert.Equal(t, 2, queries)
}
