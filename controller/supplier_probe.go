package controller

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/relay"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

// supplierTaskProbe persists its public task ID before dispatch. A crash after
// sending can leave an unknown result, but can never automatically repeat the
// billable submission. Draft version fencing also prevents a late old probe
// from validating a changed credential or endpoint.
func supplierTaskProbe(ctx context.Context, draft *model.SupplierChannelDraft, channel *model.Channel, userID int, modelName string, keyIndex int) string {
	probes := map[string]string{}
	if draft.ProbeTaskIDs != "" && common.UnmarshalJsonStr(draft.ProbeTaskIDs, &probes) != nil {
		return "result_unknown"
	}
	probeKey := modelName + ":" + strconv.Itoa(keyIndex)
	if taskID := probes[probeKey]; taskID != "" {
		var task model.Task
		if model.DB.Where("task_id = ? AND channel_id = ?", taskID, channel.Id).First(&task).Error != nil {
			return "result_unknown"
		}
		if task.PrivateData.ChannelAttempt == nil || task.PrivateData.ChannelAttempt.SupplierID != draft.SupplierID {
			return "result_unknown"
		}
		observation := task.PrivateData.ChannelAttempt.Observation
		if observation.Authoritative && observation.Status == "succeeded" {
			return "validated"
		}
		if observation.Authoritative && observation.Status == "failed" {
			return "failed"
		}
		// Polling uses the frozen, encrypted task connection and does not consume
		// a second submit permit, even if the current channel is paused.
		if service.GetTaskAdaptorFunc != nil {
			_ = service.PollTaskChannelOnce(ctx, &task)
		}
		if model.DB.Where("task_id = ?", taskID).First(&task).Error != nil {
			return "result_unknown"
		}
		observation = task.PrivateData.ChannelAttempt.Observation
		if observation.Authoritative && observation.Status == "succeeded" {
			return "validated"
		}
		if observation.Authoritative && observation.Status == "failed" {
			return "failed"
		}
		if observation.Status == "unknown" {
			return "result_unknown"
		}
		return "waiting"
	}

	generation := jsplugin.DefaultRegistry.Generation()
	pluginKey := channel.GetSetting().TaskPluginKey
	var pinned jsplugin.PinnedEndpoint
	var native jsplugin.PinnedRoute
	var path string
	// The installed driver interprets its own canonical protocol request.
	// No vendor schema, credentials, or supplier-provided rewrite is inferred.
	for _, candidatePath := range []string{"/v1/responses", "/v1/images/generations", "/v1/videos"} {
		for _, candidate := range generation.LookupEndpointCandidates(http.MethodPost, candidatePath, modelName) {
			if candidate.Plugin.Meta.Key != pluginKey {
				continue
			}
			pinned = jsplugin.PinnedEndpoint{Generation: generation, Plugin: candidate.Plugin, Protocol: candidate.Protocol, Operation: candidate.Operation, Model: modelName, Candidates: []jsplugin.ProtocolBinding{candidate}}
			path = candidatePath
			break
		}
		if path != "" {
			break
		}
	}
	if path == "" {
		plugin, ok := generation.Get(pluginKey)
		if ok {
			for _, route := range plugin.Meta.Routes {
				if route.Method != http.MethodPost || route.Type != jsplugin.RouteTypeSubmit || strings.ContainsAny(route.Path, ":*") {
					continue
				}
				if len(route.Models) > 0 && !slices.Contains(route.Models, modelName) {
					continue
				}
				native = jsplugin.PinnedRoute{Generation: generation, Plugin: plugin, Route: route}
				path = route.Path
				break
			}
		}
	}
	if path == "" {
		return "failed"
	}
	body := map[string]any{"model": modelName, "input": "A blue square on a plain white background", "prompt": "A blue square on a plain white background"}
	if path == "/v1/images/generations" {
		body = map[string]any{"model": modelName, "prompt": "A blue square on a plain white background", "n": 1}
	}
	if pinned.Protocol == "openai_responses" && pinned.Plugin.Meta.ProtocolSupports(pinned.Protocol, "background") {
		body["background"] = true
	}
	encoded, err := common.Marshal(body)
	if err != nil {
		return "failed"
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequestWithContext(ctx, http.MethodPost, path, bytes.NewReader(encoded))
	c.Request.Header.Set("Content-Type", "application/json")
	cache, err := model.GetUserCache(userID)
	if err != nil {
		return "failed"
	}
	cache.WriteContext(c)
	c.Set("id", userID)
	common.SetContextKey(c, constant.ContextKeyRequestStartTime, time.Now())
	common.SetContextKey(c, constant.ContextKeyUsingGroup, channel.Group)
	common.SetContextKey(c, constant.ContextKeyTokenGroup, channel.Group)
	if native.Plugin != nil {
		c.Set(jsplugin.ContextKeyPinnedRoute, native)
		middleware.PrepareTaskPluginRoute()(c)
	} else {
		c.Set(jsplugin.ContextKeyPinnedEndpoint, pinned)
		middleware.PrepareTaskPluginEndpoint()(c)
	}
	if c.IsAborted() || c.GetString("resolved_task_model") != modelName {
		return "failed"
	}
	if middleware.SetupContextForSelectedChannel(c, channel, modelName) != nil {
		return "failed"
	}
	info, err := relaycommon.GenRelayInfo(c, types.RelayFormatTask, nil, nil)
	if err != nil {
		return "failed"
	}
	info.InitChannelMeta(c)
	info.IsChannelTest, info.OriginModelName, info.UpstreamModelName = true, modelName, modelName
	if info.TaskRelayInfo == nil {
		info.TaskRelayInfo = &relaycommon.TaskRelayInfo{}
	}
	info.PublicTaskID, info.Action = model.GenerateTaskID(), c.GetString("task_action")
	info.InitChannelMeta(c)
	if service.PrepareChannelLimitAttempt(info, channel) != nil {
		return "waiting"
	}
	probes[probeKey] = info.PublicTaskID
	encoded, err = common.Marshal(probes)
	if err != nil {
		return "failed"
	}
	result := model.DB.Model(&model.SupplierChannelDraft{}).Where("channel_id = ? AND supplier_id = ? AND version = ? AND status = ?", draft.ChannelID, draft.SupplierID, draft.Version, "testing").Where("COALESCE(probe_task_ids, '') = ?", draft.ProbeTaskIDs).Update("probe_task_ids", string(encoded))
	if result.Error != nil || result.RowsAffected != 1 {
		return "waiting"
	}
	draft.ProbeTaskIDs = string(encoded)
	submission, taskErr := relay.RelayTaskSubmit(c, info)
	if taskErr != nil || submission == nil {
		if info.ChannelLimitAttempt != nil && !info.ChannelLimitAttempt.Sent {
			delete(probes, probeKey)
			encoded, _ = common.Marshal(probes)
			_ = model.DB.Model(&model.SupplierChannelDraft{}).Where("channel_id = ? AND version = ?", draft.ChannelID, draft.Version).Where("COALESCE(probe_task_ids, '') = ?", draft.ProbeTaskIDs).Update("probe_task_ids", string(encoded)).Error
			draft.ProbeTaskIDs = string(encoded)
			return "failed"
		}
		return "result_unknown"
	}
	task := model.InitTask(submission.Platform, info)
	task.PrivateData.UpstreamTaskID = submission.UpstreamTaskID
	task.PrivateData.PluginState = submission.PluginState
	task.Data, task.Action = submission.TaskData, info.Action
	// No consumer is billed for a controlled probe. Procurement is independently
	// frozen in TaskChannelSnapshot and settles even though customer quota is zero.
	task.Quota = 0
	if immediate := submission.Immediate; immediate != nil {
		task.Status, task.Progress = model.TaskStatus(immediate.Status), immediate.Progress
		if task.Status == model.TaskStatusSuccess || task.Status == model.TaskStatusFailure {
			task.FinishTime = time.Now().Unix()
		}
	}
	if err := task.InsertWithContext(context.WithoutCancel(ctx)); err != nil {
		return "result_unknown"
	}
	if task.PrivateData.ChannelAttempt == nil {
		return "result_unknown"
	}
	if err := service.FlushTaskChannelObservation(context.WithoutCancel(ctx), task); err != nil {
		return "result_unknown"
	}
	observation := task.PrivateData.ChannelAttempt.Observation
	if observation.Authoritative && observation.Status == "succeeded" {
		return "validated"
	}
	if observation.Authoritative && observation.Status == "failed" {
		return "failed"
	}
	return "waiting"
}

func supplierMidjourneyProbe(ctx context.Context, draft *model.SupplierChannelDraft, channel *model.Channel, userID int, modelName string, keyIndex int) string {
	if modelName != "mj_imagine" && modelName != "mj_shorten" {
		return "failed"
	}
	probes := map[string]string{}
	if draft.ProbeTaskIDs != "" && common.UnmarshalJsonStr(draft.ProbeTaskIDs, &probes) != nil {
		return "result_unknown"
	}
	key := modelName + ":" + strconv.Itoa(keyIndex)
	var task *model.Midjourney
	if reference := probes[key]; reference != "" {
		id, err := strconv.Atoi(strings.TrimPrefix(reference, "mj:"))
		if err != nil || id <= 0 {
			return "result_unknown"
		}
		task = &model.Midjourney{}
		if model.DB.First(task, "id = ? AND channel_id = ?", id, channel.Id).Error != nil {
			return "result_unknown"
		}
		snapshot, err := task.ChannelAttempt()
		if err != nil || snapshot == nil || snapshot.SupplierID != draft.SupplierID {
			return "result_unknown"
		}
		if !snapshot.Observation.Authoritative || snapshot.Observation.Status != "succeeded" && snapshot.Observation.Status != "failed" {
			_ = service.RefreshMidjourneyTask(ctx, task)
		}
	} else {
		// Unlike modern task endpoints MJ generates its vendor ID on submit.
		// An unresolved dispatch marker must never trigger an automatic resend.
		probes[key] = "submitting"
		encoded, err := common.Marshal(probes)
		if err != nil {
			return "failed"
		}
		write := model.DB.Model(&model.SupplierChannelDraft{}).Where("channel_id = ? AND supplier_id = ? AND version = ? AND status = ?", draft.ChannelID, draft.SupplierID, draft.Version, "testing").Where("COALESCE(probe_task_ids, '') = ?", draft.ProbeTaskIDs).Update("probe_task_ids", string(encoded))
		if write.Error != nil || write.RowsAffected != 1 {
			return "waiting"
		}
		draft.ProbeTaskIDs = string(encoded)
		task, err = relay.SubmitMidjourneyChannelProbe(ctx, channel, userID, modelName)
		if err != nil || task == nil {
			return "result_unknown"
		}
		probes[key] = "mj:" + strconv.Itoa(task.Id)
		encoded, err = common.Marshal(probes)
		if err != nil {
			return "result_unknown"
		}
		linked := model.DB.Model(&model.SupplierChannelDraft{}).Where("channel_id = ? AND version = ?", draft.ChannelID, draft.Version).Where("COALESCE(probe_task_ids, '') = ?", draft.ProbeTaskIDs).Update("probe_task_ids", string(encoded))
		if linked.Error != nil || linked.RowsAffected != 1 {
			return "result_unknown"
		}
		draft.ProbeTaskIDs = string(encoded)
	}
	snapshot, err := task.ChannelAttempt()
	if err != nil || snapshot == nil {
		return "result_unknown"
	}
	if snapshot.Observation.Authoritative && snapshot.Observation.Status == "succeeded" {
		return "validated"
	}
	if snapshot.Observation.Authoritative && snapshot.Observation.Status == "failed" {
		return "failed"
	}
	if snapshot.Observation.Status == "unknown" {
		return "result_unknown"
	}
	return "waiting"
}
