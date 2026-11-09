package jsplugin

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"net/http"
	"time"

	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/abema/go-mp4"
)

// Probe only when a priced output fact is absent, after generation succeeded.
func (a *TaskAdaptor) completeVideoUsage(ctx context.Context, task *model.Task, result *relaycommon.TaskInfo) {
	if a.plugin.Meta.Key != "sora" || task == nil || result.Status != model.TaskStatusSuccess ||
		task.PrivateData.BillingContext == nil || task.PrivateData.BillingContext.TieredSnapshot == nil {
		return
	}
	snapshot := task.PrivateData.BillingContext.TieredSnapshot
	if !snapshot.VideoUsagePending {
		return
	}
	keys := billingexpr.UsedUsageKeys(snapshot.ExprString)
	if (!keys["resolution"] || result.UsageFacts["resolution"] != nil) &&
		(!keys["seconds"] || result.UsageFacts["seconds"] != nil) {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	key, baseURL, proxy := a.queryCredentials()
	contentAdaptor := a
	if task.PrivateData.Key != "" {
		key = task.PrivateData.Key
		if a.info != nil && a.info.HasChannelMeta() {
			copyAdaptor, copyInfo, copyMeta := *a, *a.info, *a.info.ChannelMeta
			copyInfo.ChannelMeta = &copyMeta
			copyInfo.ApiKey = key
			copyAdaptor.info = &copyInfo
			contentAdaptor = &copyAdaptor
		}
	}
	request, err := contentAdaptor.BuildContentRequest(ctx, task, "video", channel.TaskArtifactClientRequest{Method: http.MethodGet})
	if err != nil || request == nil || request.Method != http.MethodGet || request.Credentialless || len(request.ContentPath) != 0 {
		logger.LogWarn(ctx, "video usage could not be measured; retaining estimated quota")
		return
	}
	if err := service.ValidateSSRFProtectedFetchURL(request.URL); err != nil {
		logger.LogWarn(ctx, "video usage download blocked; retaining estimated quota")
		return
	}
	resp, err := a.doFetchDescriptor(ctx, baseURL, key, proxy, requestDescriptor{
		URL: request.URL, Method: request.Method, Headers: request.Headers,
	})
	if err != nil {
		logger.LogWarn(ctx, "video usage download failed; retaining estimated quota")
		return
	}
	defer resp.Body.Close()
	limit := min(maxInlineFileBytes(), int64(64<<20))
	if resp.StatusCode != http.StatusOK || resp.ContentLength > limit {
		logger.LogWarn(ctx, "video usage download rejected; retaining estimated quota")
		return
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || int64(len(data)) > limit {
		logger.LogWarn(ctx, "video usage download incomplete; retaining estimated quota")
		return
	}
	measured, err := measureMP4Video(data)
	if err != nil {
		logger.LogWarn(ctx, "video metadata unavailable; retaining estimated quota")
		return
	}
	facts := make(map[string]any, len(measured)+len(result.UsageFacts))
	maps.Copy(facts, measured)
	// Explicit upstream usage takes precedence over media metadata.
	maps.Copy(facts, result.UsageFacts)
	if err := a.applyCompletionUsageFacts(result, facts, task.Properties.UpstreamModelName, task.Properties.OriginModelName); err != nil {
		logger.LogWarn(ctx, "video metadata rejected; retaining estimated quota")
	}
}

// Read fixed-size movie and track headers, without decoding sample tables.
func measureMP4Video(data []byte) (map[string]any, error) {
	var movie *mp4.Mvhd
	var video *mp4.Tkhd
	visited := 0
	_, err := mp4.ReadBoxStructure(bytes.NewReader(data), func(handle *mp4.ReadHandle) (any, error) {
		visited++
		if visited > 512 {
			return nil, fmt.Errorf("video metadata box limit exceeded")
		}
		if (len(handle.Path) == 1 && handle.BoxInfo.Type == mp4.BoxTypeMoov()) ||
			(len(handle.Path) == 2 && handle.Path[0] == mp4.BoxTypeMoov() && handle.BoxInfo.Type == mp4.BoxTypeTrak()) {
			return handle.Expand()
		}
		if !((len(handle.Path) == 2 && handle.Path[0] == mp4.BoxTypeMoov() && handle.BoxInfo.Type == mp4.BoxTypeMvhd()) ||
			(len(handle.Path) == 3 && handle.Path[0] == mp4.BoxTypeMoov() && handle.Path[1] == mp4.BoxTypeTrak() && handle.BoxInfo.Type == mp4.BoxTypeTkhd())) {
			return nil, nil
		}
		if handle.BoxInfo.Size > 256 {
			return nil, fmt.Errorf("video metadata header exceeds size limit")
		}
		box, _, err := handle.ReadPayload()
		if err != nil {
			return nil, err
		}
		switch header := box.(type) {
		case *mp4.Mvhd:
			movie = header
		case *mp4.Tkhd:
			if header.Width > 0 && header.Height > 0 {
				if video != nil && (video.Width != header.Width || video.Height != header.Height) {
					return nil, fmt.Errorf("video contains ambiguous output dimensions")
				}
				video = header
			}
		}
		return nil, nil
	})
	if err != nil {
		return nil, err
	}
	if movie == nil || video == nil || movie.Timescale == 0 {
		return nil, fmt.Errorf("video metadata missing")
	}
	facts := map[string]any{}
	seconds := float64(video.GetDuration()) / float64(movie.Timescale)
	if seconds > 0 && seconds <= relaycommon.MaxTaskDurationSeconds {
		facts["seconds"] = seconds
	}
	shortEdge := min(video.GetWidth(), video.GetHeight())
	if shortEdge == 480 || shortEdge == 720 || shortEdge == 1080 {
		facts["resolution"] = fmt.Sprintf("%.0fp", shortEdge)
	}
	return facts, nil
}
