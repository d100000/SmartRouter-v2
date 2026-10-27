package model

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/perf_metrics_setting"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestMain(m *testing.M) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		panic("failed to open test db: " + err.Error())
	}
	DB = db
	LOG_DB = db

	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	common.RedisEnabled = false
	common.BatchUpdateEnabled = false
	common.LogConsumeEnabled = true
	initCol()

	sqlDB, err := db.DB()
	if err != nil {
		panic("failed to get sql.DB: " + err.Error())
	}
	sqlDB.SetMaxOpenConns(1)

	if err := db.AutoMigrate(
		&Task{},
		&User{},
		&UserSession{},
		&AuthFlow{},
		&ExternalIdentityClaim{},
		&Token{},
		&PasskeyCredential{},
		&TwoFA{},
		&TwoFABackupCode{},
		&Log{},
		&Channel{},
		&UpstreamSupplier{},
		&UpstreamCredential{},
		&UpstreamCredentialVersion{},
		&ChannelCredentialBinding{},
		&UpstreamCredentialFingerprintSecret{},
		&QuotaData{},
		&Ability{},
		&TopUp{},
		&SubscriptionPlan{},
		&SubscriptionOrder{},
		&UserSubscription{},
		&UserOAuthBinding{},
		&PerfMetric{},
		&PerfMetricEvent{},
		&SystemInstance{},
		&SystemTask{},
		&SystemTaskLock{},
		&UserAccessToken{},
	); err != nil {
		panic("failed to migrate: " + err.Error())
	}

	os.Exit(m.Run())
}

func truncateTables(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		DB.Exec("DELETE FROM tasks")
		DB.Exec("DELETE FROM auth_flows")
		DB.Exec("DELETE FROM external_identity_claims")
		DB.Exec("DELETE FROM user_sessions")
		DB.Exec("DELETE FROM passkey_credentials")
		DB.Exec("DELETE FROM two_fa_backup_codes")
		DB.Exec("DELETE FROM two_fas")
		DB.Exec("DELETE FROM tokens")
		DB.Exec("DELETE FROM user_access_tokens")
		DB.Exec("DELETE FROM user_oauth_bindings")
		DB.Exec("DELETE FROM users")
		DB.Exec("DELETE FROM logs")
		DB.Exec("DELETE FROM channels")
		DB.Exec("DELETE FROM channel_credential_bindings")
		DB.Exec("DELETE FROM upstream_credential_versions")
		DB.Exec("DELETE FROM upstream_credentials")
		DB.Exec("DELETE FROM upstream_suppliers")
		DB.Exec("DELETE FROM quota_data")
		DB.Exec("DELETE FROM abilities")
		DB.Exec("DELETE FROM top_ups")
		DB.Exec("DELETE FROM subscription_orders")
		DB.Exec("DELETE FROM subscription_plans")
		DB.Exec("DELETE FROM user_subscriptions")
		DB.Exec("DELETE FROM perf_metrics")
		DB.Exec("DELETE FROM perf_metric_events")
		DB.Exec("DELETE FROM system_instances")
		DB.Exec("DELETE FROM system_task_locks")
		DB.Exec("DELETE FROM system_tasks")
	})
}

func insertTask(t *testing.T, task *Task) {
	t.Helper()
	task.CreatedAt = time.Now().Unix()
	task.UpdatedAt = time.Now().Unix()
	require.NoError(t, DB.Create(task).Error)
}

func TestGetTaskForProtocolObservationScopesOwnerAndPlatform(t *testing.T) {
	truncateTables(t)
	task := &Task{
		TaskID:   "task_protocol_scope",
		UserId:   7,
		Platform: "plugin-a",
		Status:   TaskStatusInProgress,
	}
	insertTask(t, task)

	got, exists, err := GetTaskForProtocolObservation(context.Background(), 7, "plugin-a", task.TaskID)
	require.NoError(t, err)
	require.True(t, exists)
	assert.Equal(t, task.ID, got.ID)

	for _, query := range []struct {
		userID   int
		platform string
	}{
		{userID: 8, platform: "plugin-a"},
		{userID: 7, platform: "plugin-b"},
	} {
		got, exists, err = GetTaskForProtocolObservation(context.Background(), query.userID, constant.TaskPlatform(query.platform), task.TaskID)
		require.NoError(t, err)
		assert.False(t, exists)
		assert.Nil(t, got)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err = GetTaskForProtocolObservation(cancelled, 7, "plugin-a", task.TaskID)
	require.ErrorIs(t, err, context.Canceled)
}

// ---------------------------------------------------------------------------
// Snapshot / Equal — pure logic tests (no DB)
// ---------------------------------------------------------------------------

func TestSnapshotEqual_Same(t *testing.T) {
	s := taskSnapshot{
		Status:     TaskStatusInProgress,
		Progress:   "50%",
		StartTime:  1000,
		FinishTime: 0,
		FailReason: "",
		ResultURL:  "",
		Data:       json.RawMessage(`{"key":"value"}`),
	}
	assert.True(t, s.Equal(s))
}

func TestSnapshotEqual_DifferentStatus(t *testing.T) {
	a := taskSnapshot{Status: TaskStatusInProgress, Data: json.RawMessage(`{}`)}
	b := taskSnapshot{Status: TaskStatusSuccess, Data: json.RawMessage(`{}`)}
	assert.False(t, a.Equal(b))
}

func TestSnapshotEqual_DifferentProgress(t *testing.T) {
	a := taskSnapshot{Status: TaskStatusInProgress, Progress: "30%", Data: json.RawMessage(`{}`)}
	b := taskSnapshot{Status: TaskStatusInProgress, Progress: "60%", Data: json.RawMessage(`{}`)}
	assert.False(t, a.Equal(b))
}

func TestSnapshotEqual_DifferentData(t *testing.T) {
	a := taskSnapshot{Status: TaskStatusInProgress, Data: json.RawMessage(`{"a":1}`)}
	b := taskSnapshot{Status: TaskStatusInProgress, Data: json.RawMessage(`{"a":2}`)}
	assert.False(t, a.Equal(b))
}

func TestSnapshotEqual_NilVsEmpty(t *testing.T) {
	a := taskSnapshot{Status: TaskStatusInProgress, Data: nil}
	b := taskSnapshot{Status: TaskStatusInProgress, Data: json.RawMessage{}}
	// bytes.Equal(nil, []byte{}) == true
	assert.True(t, a.Equal(b))
}

func TestSnapshotEqual_PluginStateAndPollFailures(t *testing.T) {
	base := taskSnapshot{
		Status:       TaskStatusInProgress,
		PluginState:  json.RawMessage(`{"req_key":"a"}`),
		PollFailures: 2,
	}
	assert.True(t, base.Equal(taskSnapshot{
		Status:       TaskStatusInProgress,
		PluginState:  json.RawMessage(`{"req_key":"a"}`),
		PollFailures: 2,
	}))
	assert.False(t, base.Equal(taskSnapshot{
		Status:       TaskStatusInProgress,
		PluginState:  json.RawMessage(`{"req_key":"b"}`),
		PollFailures: 2,
	}))
	assert.False(t, base.Equal(taskSnapshot{
		Status:       TaskStatusInProgress,
		PluginState:  json.RawMessage(`{"req_key":"a"}`),
		PollFailures: 3,
	}))
}

func TestSnapshot_Roundtrip(t *testing.T) {
	task := &Task{
		Status:     TaskStatusInProgress,
		Progress:   "42%",
		StartTime:  1234,
		FinishTime: 5678,
		FailReason: "timeout",
		PrivateData: TaskPrivateData{
			ResultURL:    "https://example.com/result.mp4",
			PluginState:  json.RawMessage(`{"req_key":"keep"}`),
			PollFailures: 3,
		},
		Data: json.RawMessage(`{"model":"test-model"}`),
	}
	snap := task.Snapshot()
	assert.Equal(t, task.Status, snap.Status)
	assert.Equal(t, task.Progress, snap.Progress)
	assert.Equal(t, task.StartTime, snap.StartTime)
	assert.Equal(t, task.FinishTime, snap.FinishTime)
	assert.Equal(t, task.FailReason, snap.FailReason)
	assert.Equal(t, task.PrivateData.ResultURL, snap.ResultURL)
	assert.JSONEq(t, string(task.Data), string(snap.Data))
	assert.Equal(t, task.PrivateData.PluginState, snap.PluginState)
	assert.Equal(t, task.PrivateData.PollFailures, snap.PollFailures)
}

// ---------------------------------------------------------------------------
// UpdateWithStatus CAS — DB integration tests
// ---------------------------------------------------------------------------

func TestUpdateWithStatus_Win(t *testing.T) {
	truncateTables(t)

	task := &Task{
		TaskID:   "task_cas_win",
		Status:   TaskStatusInProgress,
		Progress: "50%",
		Data:     json.RawMessage(`{}`),
	}
	insertTask(t, task)

	task.Status = TaskStatusSuccess
	task.Progress = "100%"
	won, err := task.UpdateWithStatus(TaskStatusInProgress)
	require.NoError(t, err)
	assert.True(t, won)

	var reloaded Task
	require.NoError(t, DB.First(&reloaded, task.ID).Error)
	assert.EqualValues(t, TaskStatusSuccess, reloaded.Status)
	assert.Equal(t, "100%", reloaded.Progress)
}

func TestUpdateWithStatus_Lose(t *testing.T) {
	truncateTables(t)

	task := &Task{
		TaskID: "task_cas_lose",
		Status: TaskStatusFailure,
		Data:   json.RawMessage(`{}`),
	}
	insertTask(t, task)

	task.Status = TaskStatusSuccess
	won, err := task.UpdateWithStatus(TaskStatusInProgress) // wrong fromStatus
	require.NoError(t, err)
	assert.False(t, won)

	var reloaded Task
	require.NoError(t, DB.First(&reloaded, task.ID).Error)
	assert.EqualValues(t, TaskStatusFailure, reloaded.Status) // unchanged
}

func TestUpdateWithStatus_ConcurrentWinner(t *testing.T) {
	truncateTables(t)

	task := &Task{
		TaskID: "task_cas_race",
		Status: TaskStatusInProgress,
		Quota:  1000,
		Data:   json.RawMessage(`{}`),
	}
	insertTask(t, task)

	const goroutines = 5
	wins := make([]bool, goroutines)
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := range goroutines {
		go func(idx int) {
			defer wg.Done()
			t := &Task{}
			*t = Task{
				ID:       task.ID,
				TaskID:   task.TaskID,
				Status:   TaskStatusSuccess,
				Progress: "100%",
				Quota:    task.Quota,
				Data:     json.RawMessage(`{}`),
			}
			t.CreatedAt = task.CreatedAt
			t.UpdatedAt = time.Now().Unix()
			won, err := t.UpdateWithStatus(TaskStatusInProgress)
			if err == nil {
				wins[idx] = won
			}
		}(i)
	}
	wg.Wait()

	winCount := 0
	for _, w := range wins {
		if w {
			winCount++
		}
	}
	assert.Equal(t, 1, winCount, "exactly one goroutine should win the CAS")
}

func TestUpdateWithStatus_PersistsPluginStateAndPollFailures(t *testing.T) {
	truncateTables(t)

	task := &Task{
		TaskID: "task_cas_plugin_state",
		Status: TaskStatusInProgress,
		Data:   json.RawMessage(`{}`),
		PrivateData: TaskPrivateData{
			PluginState:  json.RawMessage(`{"req_key":"old"}`),
			PollFailures: 1,
		},
	}
	insertTask(t, task)

	task.PrivateData.PluginState = json.RawMessage(`{"req_key":"new"}`)
	task.PrivateData.PollFailures = 4
	won, err := task.UpdateWithStatus(TaskStatusInProgress)
	require.NoError(t, err)
	require.True(t, won)

	var reloaded Task
	require.NoError(t, DB.First(&reloaded, task.ID).Error)
	assert.EqualValues(t, TaskStatusInProgress, reloaded.Status)
	assert.JSONEq(t, `{"req_key":"new"}`, string(reloaded.PrivateData.PluginState))
	assert.Equal(t, 4, reloaded.PrivateData.PollFailures)
}

func TestTerminalTaskPerformanceSurvivesCollectorFailure(t *testing.T) {
	truncateTables(t)
	previous := perf_metrics_setting.GetSetting()
	config.UpdateConfigFromMap(config.GlobalConfig.Get("perf_metrics_setting"), map[string]string{"enabled": "true"})
	t.Cleanup(func() {
		config.UpdateConfigFromMap(config.GlobalConfig.Get("perf_metrics_setting"), map[string]string{"enabled": strconv.FormatBool(previous.Enabled)})
	})
	const rejectCallback = "test:reject_task_performance_event"
	reject := true
	require.NoError(t, DB.Callback().Create().Before("gorm:create").Register(rejectCallback, func(tx *gorm.DB) {
		if tx.Statement.Table == (PerfMetricEvent{}).TableName() && reject {
			tx.AddError(errors.New("metrics unavailable"))
		}
	}))
	t.Cleanup(func() { DB.Callback().Create().Remove(rejectCallback) })

	now := time.Now().Unix()
	zero := int64(0)
	task := &Task{TaskID: "terminal-with-sample", Status: TaskStatusInProgress, Group: "retired-group", SubmitTime: now - 20, StartTime: now - 10, Quota: 91,
		Properties: Properties{OriginModelName: "private-model"}, PrivateData: TaskPrivateData{PerformanceOutputTokens: &zero}}
	insertTask(t, task)
	task.Status = TaskStatusSuccess
	task.FinishTime = now
	won, err := task.UpdateWithStatus(TaskStatusInProgress)
	require.NoError(t, err, "the collector cannot block the terminal state or billing")
	require.True(t, won)
	var persisted Task
	require.NoError(t, DB.First(&persisted, task.ID).Error)
	assert.Equal(t, TaskStatus(TaskStatusSuccess), persisted.Status)
	assert.Equal(t, 91, persisted.Quota)
	assert.True(t, persisted.PerformanceSamplePending)
	require.NotNil(t, persisted.PrivateData.PerformanceOutputTokens)
	assert.Zero(t, *persisted.PrivateData.PerformanceOutputTokens)
	require.Positive(t, persisted.FinishTime)
	terminalTime := persisted.FinishTime

	staged, err := StagePerfMetricTaskEvents(context.Background(), DB)
	require.ErrorContains(t, err, "metrics unavailable")
	assert.Zero(t, staged)
	require.NoError(t, DB.First(&persisted, task.ID).Error)
	assert.True(t, persisted.PerformanceSamplePending, "failed outbox insertion rolls the marker claim back")
	assert.Equal(t, terminalTime, persisted.FinishTime)

	reject = false
	staged, err = StagePerfMetricTaskEvents(context.Background(), DB)
	require.NoError(t, err)
	assert.Equal(t, 1, staged)
	var events []PerfMetricEvent
	require.NoError(t, DB.Find(&events).Error)
	require.Len(t, events, 1)
	assert.Equal(t, "private-model", events[0].ModelName)
	assert.Equal(t, "retired-group", events[0].Group)
	assert.Equal(t, terminalTime, events[0].SampleAt)
	assert.Equal(t, int64(0), events[0].OutputTokens)
	assert.True(t, events[0].OutputTokensKnown)
	assert.Equal(t, int64(10_000), events[0].GenerationMs, "explicit zero output retains a valid throughput denominator")
	missingDuration := *task
	missingDuration.StartTime = 0
	hundred := int64(100)
	missingDuration.PrivateData.PerformanceOutputTokens = &hundred
	measurement := missingDuration.PerformanceMetricEvent()
	require.NotNil(t, measurement)
	assert.True(t, measurement.OutputTokensKnown, "known output remains known when its duration is missing")
	assert.Equal(t, hundred, measurement.OutputTokens)
	assert.Zero(t, measurement.GenerationMs, "submission time is not a generation duration")
	require.NoError(t, DB.First(&persisted, task.ID).Error)
	assert.False(t, persisted.PerformanceSamplePending)

	staged, err = StagePerfMetricTaskEvents(context.Background(), DB)
	require.NoError(t, err)
	assert.Zero(t, staged)
	won, err = task.UpdateWithStatus(TaskStatusInProgress)
	require.NoError(t, err)
	assert.False(t, won, "a duplicate terminal CAS cannot reactivate the marker")

	immediate := &Task{TaskID: task.TaskID, Status: TaskStatusFailure, Properties: Properties{OriginModelName: "private-model"}}
	require.NoError(t, immediate.Insert())
	require.Positive(t, immediate.FinishTime)
	assert.True(t, immediate.PerformanceSamplePending)
	staged, err = StagePerfMetricTaskEvents(context.Background(), DB)
	require.NoError(t, err)
	assert.Equal(t, 1, staged)
	require.NoError(t, DB.Order("id").Find(&events).Error)
	require.Len(t, events, 2)
	assert.NotEqual(t, events[0].ID, events[1].ID, "legacy public task IDs need not be unique")

	legacy := &Task{TaskID: "already-accounted-terminal", Status: TaskStatusSuccess, FinishTime: now, Properties: Properties{OriginModelName: "private-model"}}
	require.NoError(t, DB.Create(legacy).Error)
	staged, err = StagePerfMetricTaskEvents(context.Background(), DB)
	require.NoError(t, err)
	assert.Zero(t, staged, "upgrading does not replay already-terminal historical tasks")

	bad := &Task{TaskID: "bad-dimension", Status: TaskStatusFailure, Properties: Properties{OriginModelName: strings.Repeat("x", 129)}}
	require.NoError(t, bad.Insert())
	future := &Task{TaskID: "bad-future-time", Status: TaskStatusFailure, FinishTime: now + 120, Properties: Properties{OriginModelName: "future-task"}}
	require.NoError(t, future.Insert())
	good := &Task{TaskID: "after-bad-dimension", Status: TaskStatusFailure, Properties: Properties{OriginModelName: "still-collectable"}}
	require.NoError(t, good.Insert())
	staged, err = StagePerfMetricTaskEvents(context.Background(), DB)
	require.ErrorContains(t, err, "invalid_dimensions")
	assert.Equal(t, 1, staged, "an invalid sample does not block later valid tasks")
	persisted = Task{}
	require.NoError(t, DB.First(&persisted, bad.ID).Error)
	assert.True(t, persisted.PerformanceSamplePending, "quarantine preserves the pending data")
	assert.Equal(t, "invalid_dimensions", persisted.PerformanceSampleError)
	persisted = Task{}
	require.NoError(t, DB.First(&persisted, future.ID).Error)
	assert.True(t, persisted.PerformanceSamplePending)
	assert.Equal(t, "invalid_measurements", persisted.PerformanceSampleError)
	assert.Equal(t, now+120, persisted.FinishTime, "quarantine does not rewrite provider facts")
	staged, err = StagePerfMetricTaskEvents(context.Background(), DB)
	require.NoError(t, err)
	assert.Zero(t, staged, "quarantined tasks are not retried forever ahead of healthy tasks")
}
