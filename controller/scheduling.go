package controller

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/scheduler"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
)

var schedulingStartedAt = time.Now().Unix()

type schedulingDashboard struct {
	scheduler.Snapshot
	Groups              []string                 `json:"groups"`
	Models              []string                 `json:"models"`
	Group               string                   `json:"group"`
	Model               string                   `json:"model"`
	Stream              bool                     `json:"stream"`
	RefreshedAt         int64                    `json:"refreshed_at"`
	StartedAt           int64                    `json:"started_at"`
	ActivationThreshold int                      `json:"activation_threshold"`
	Config              service.SchedulingConfig `json:"config"`
}

func GetScheduling(c *gin.Context) {
	stream := true
	if raw := c.Query("stream"); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "stream must be true or false"})
			return
		}
		stream = value
	}
	if err := service.SyncSchedulingConfig(); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "message": err.Error()})
		return
	}
	channels, err := model.GetAllChannels(0, -1, false, true)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": "Unable to load scheduling channels"})
		return
	}
	groups := make([]string, 0)
	for _, channel := range channels {
		groups = append(groups, channel.GetGroups()...)
	}
	slices.Sort(groups)
	groups = slices.Compact(groups)
	recentKeys := scheduler.Default.RecentBusinessKeys()
	group := strings.TrimSpace(c.Query("group"))
	if group != "" && !slices.Contains(groups, group) {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "code": "scheduling_selection_unavailable", "message": "Unknown scheduling group"})
		return
	}
	if group == "" {
		for _, recent := range recentKeys {
			if slices.ContainsFunc(channels, func(channel *model.Channel) bool { return schedulingChannelMatches(channel, recent) }) {
				group = recent.Group
				break
			}
		}
		if group == "" && len(groups) > 0 {
			group = groups[0]
		}
	}
	models := make([]string, 0)
	for _, channel := range channels {
		if slices.Contains(channel.GetGroups(), group) {
			models = append(models, channel.GetModels()...)
		}
	}
	// Concrete model names matched by a native routing pattern keep their own
	// learning key, so observed names must also be selectable in the dashboard.
	for _, observed := range scheduler.Default.Keys() {
		if observed.Group == group && observed.Model != "" && slices.ContainsFunc(channels, func(channel *model.Channel) bool { return schedulingChannelMatches(channel, observed) }) {
			models = append(models, observed.Model)
		}
	}
	slices.Sort(models)
	models = slices.Compact(models)
	modelName := strings.TrimSpace(c.Query("model"))
	if modelName != "" && !slices.Contains(models, modelName) {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "code": "scheduling_selection_unavailable", "message": "Unknown model in this group"})
		return
	}
	if modelName == "" && len(models) > 0 {
		modelName = models[0]
		for _, recent := range recentKeys {
			if recent.Group == group && slices.Contains(models, recent.Model) {
				modelName = recent.Model
				break
			}
		}
	}
	key := scheduler.Key{Group: group, Model: modelName}
	candidates := make([]scheduler.Candidate, 0)
	eligible, err := model.GetSatisfiedChannelCandidates(group, modelName, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": "Unable to load routing eligibility"})
		return
	}
	eligibleByID := make(map[int]*model.Channel, len(eligible))
	for _, channel := range eligible {
		eligibleByID[channel.Id] = channel
	}
	for _, channel := range channels {
		if !schedulingChannelMatches(channel, key) {
			continue
		}
		candidate := service.SchedulerCandidate(channel)
		if routable := eligibleByID[channel.Id]; routable != nil {
			candidate = service.SchedulerCandidate(routable)
		} else {
			candidate.Excluded = true
		}
		candidates = append(candidates, candidate)
	}
	result := schedulingDashboard{
		Snapshot: scheduler.Default.Snapshot(key, candidates, stream),
		Groups:   groups, Models: models, Group: group, Model: modelName,
		Stream: stream, RefreshedAt: time.Now().Unix(), StartedAt: schedulingStartedAt,
		ActivationThreshold: 50, Config: service.GetSchedulingConfig(key),
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": result})
}

func UpdateSchedulingConfig(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4<<20)
	var config service.SchedulingConfig
	if err := c.ShouldBindJSON(&config); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "Invalid scheduling configuration"})
		return
	}
	if err := service.ValidateSchedulingConfig(config); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": err.Error()})
		return
	}
	channels, err := model.GetAllChannels(0, -1, false, true)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": "Unable to validate scheduling channels"})
		return
	}
	key := scheduler.Key{Group: config.Group, Model: config.Model}
	matching := make(map[int]bool)
	for _, channel := range channels {
		if schedulingChannelMatches(channel, key) {
			matching[channel.Id] = true
		}
	}
	if len(matching) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "No channels match this group and model"})
		return
	}
	for _, override := range config.ChannelOverrides {
		if !matching[override.ChannelID] {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "Channel override does not belong to this group and model"})
			return
		}
	}
	if err := service.SaveSchedulingConfig(config); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": "Unable to save scheduling configuration"})
		return
	}
	model.RecordLog(c.GetInt("id"), model.LogTypeManage, fmt.Sprintf("Updated scheduling configuration: group=%q model=%q enabled=%t channel_overrides=%d", config.Group, config.Model, config.Enabled, len(config.ChannelOverrides)))
	c.JSON(http.StatusOK, gin.H{"success": true, "data": config})
}

func RecoverSchedulingChannel(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 8192)
	var request struct {
		Group     string `json:"group"`
		Model     string `json:"model"`
		ChannelID int    `json:"channel_id"`
	}
	if err := c.ShouldBindJSON(&request); err != nil || request.Group == "" || request.Model == "" || request.ChannelID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "group, model and channel_id are required"})
		return
	}
	if err := service.SyncSchedulingConfig(); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "message": err.Error()})
		return
	}
	channel, err := model.GetChannelById(request.ChannelID, false)
	key := scheduler.Key{Group: request.Group, Model: request.Model}
	if err != nil || !schedulingChannelMatches(channel, key) {
		c.JSON(http.StatusNotFound, gin.H{"success": false, "message": "Channel not found in this group and model"})
		return
	}
	if channel.Status != common.ChannelStatusEnabled {
		c.JSON(http.StatusConflict, gin.H{"success": false, "message": "Disabled channels cannot be recovered by the scheduler"})
		return
	}
	eligible, err := model.GetSatisfiedChannelCandidates(key.Group, key.Model, nil)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "message": "Unable to load routing eligibility"})
		return
	}
	if !slices.ContainsFunc(eligible, func(candidate *model.Channel) bool { return candidate.Id == request.ChannelID }) {
		c.JSON(http.StatusConflict, gin.H{"success": false, "message": "Channel is not eligible for routing in this group and model"})
		return
	}
	scheduler.Default.ClearPenalty(key, request.ChannelID)
	model.RecordLog(c.GetInt("id"), model.LogTypeManage, fmt.Sprintf("Recalculated scheduling penalty: group=%q model=%q channel=%d", request.Group, request.Model, request.ChannelID))
	c.JSON(http.StatusOK, gin.H{"success": true})
}

func schedulingChannelMatches(channel *model.Channel, key scheduler.Key) bool {
	if channel == nil || !slices.Contains(channel.GetGroups(), key.Group) {
		return false
	}
	models := channel.GetModels()
	return slices.Contains(models, key.Model) || slices.Contains(models, ratio_setting.RoutingMatchModelName(key.Model))
}
