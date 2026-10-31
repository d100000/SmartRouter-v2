package controller

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

func GetErrorAnalysis(c *gin.Context) {
	var filter model.ErrorAnalysisFilter
	if err := c.ShouldBindQuery(&filter); err != nil {
		common.ApiErrorStatus(c, 400, common.NewMessage("Invalid analysis time range or filters"))
		return
	}
	if err := filter.Normalize(time.Now()); err != nil {
		common.ApiErrorStatus(c, 400, err)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()
	data, err := model.LoadErrorAnalysis(ctx, filter)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	offset, _ := strconv.Atoi(c.Query("offset"))
	offset = max(0, offset)
	category := c.Query("category")
	logs := data.Logs
	if category != "" {
		logs = []model.FailureLog{}
		for _, entry := range data.Logs {
			for _, kind := range entry.Categories {
				if kind == category {
					logs = append(logs, entry)
					break
				}
			}
		}
	}
	total := len(logs)
	offset = min(offset, total)
	data.Logs = logs[offset:min(offset+50, total)]
	if filter.RequestID == "" || c.Query("details") != "true" {
		for i := range data.Logs {
			data.Logs[i].Records = nil
			data.Logs[i].Evidence = nil
			if len([]rune(data.Logs[i].ErrorMessage)) > 512 {
				data.Logs[i].ErrorTruncated = true
			}
			data.Logs[i].ErrorMessage = model.SafeFailureFact(data.Logs[i].ErrorMessage, 512)
		}
	}
	common.ApiSuccess(c, gin.H{"snapshot": data, "log_total": total, "offset": offset})
}

func GetErrorAnalysisConfig(c *gin.Context) {
	config, err := service.GetErrorAnalysisConfig()
	if err != nil {
		common.ApiError(c, err)
		return
	}
	config.EncryptedKey = ""
	common.ApiSuccess(c, config)
}

func SaveErrorAnalysisConfig(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16*1024)
	var input struct {
		service.ErrorAnalysisConfig
		APIKey   string `json:"api_key"`
		ClearKey bool   `json:"clear_key"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		common.ApiErrorStatus(c, 400, common.NewMessage("Invalid error analysis configuration"))
		return
	}
	if err := service.SaveErrorAnalysisConfig(input.ErrorAnalysisConfig, input.APIKey, input.ClearKey); err != nil {
		common.ApiErrorStatus(c, 400, err)
		return
	}
	GetErrorAnalysisConfig(c)
}

func StartErrorAnalysis(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	var filter model.ErrorAnalysisFilter
	if err := c.ShouldBindJSON(&filter); err != nil {
		common.ApiErrorStatus(c, 400, common.NewMessage("Invalid analysis time range or filters"))
		return
	}
	task, created, err := service.StartErrorAnalysis(filter)
	if err != nil {
		common.ApiErrorStatus(c, 400, err)
		return
	}
	common.ApiSuccess(c, gin.H{"task": task.ToResponse(), "created": created})
}

func ListErrorAnalysisReports(c *gin.Context) {
	offset, _ := strconv.Atoi(c.Query("offset"))
	offset = max(0, offset)
	tasks, total, err := model.ListSystemTasks(model.SystemTaskFilter{Type: model.ErrorAnalysisTaskType}, offset, 20)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	rows := make([]model.SystemTaskResponse, 0, len(tasks))
	for _, task := range tasks {
		row := task.ToResponse()
		// History pages stay small; report text and evidence load on selection.
		row.Result = nil
		row.State = nil
		rows = append(rows, row)
	}
	common.ApiSuccess(c, gin.H{"items": rows, "total": total})
}

func GetErrorAnalysisReport(c *gin.Context) {
	task, err := model.GetSystemTaskByTaskID(c.Param("task_id"))
	if err != nil {
		common.ApiError(c, err)
		return
	}
	if task == nil || task.Type != model.ErrorAnalysisTaskType {
		common.ApiErrorStatus(c, 404, common.NewMessage("Analysis report not found"))
		return
	}
	common.ApiSuccess(c, task.ToResponse())
}

// StreamErrorAnalysis emits replaceable snapshots, so reconnecting never
// duplicates report tokens. Disconnecting a browser does not cancel the job.
func StreamErrorAnalysis(c *gin.Context) {
	task, err := model.GetSystemTaskByTaskID(c.Param("task_id"))
	if err != nil {
		common.ApiError(c, err)
		return
	}
	if task == nil || task.Type != model.ErrorAnalysisTaskType {
		common.ApiErrorStatus(c, 404, common.NewMessage("Analysis report not found"))
		return
	}
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache, no-store")
	c.Header("X-Accel-Buffering", "no")
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	deadline := time.NewTimer(5 * time.Minute)
	defer deadline.Stop()
	for {
		payload, err := common.Marshal(task.ToResponse())
		if err != nil {
			return
		}
		if _, err = fmt.Fprintf(c.Writer, "event: snapshot\ndata: %s\n\n", payload); err != nil {
			return
		}
		c.Writer.Flush()
		if task.Status == model.SystemTaskStatusFailed || task.Status == model.SystemTaskStatusSucceeded {
			return
		}
		select {
		case <-c.Request.Context().Done():
			return
		case <-deadline.C:
			return
		case <-ticker.C:
		}
		task, err = model.GetSystemTaskByTaskID(task.TaskID)
		if err != nil || task == nil {
			return
		}
	}
}
