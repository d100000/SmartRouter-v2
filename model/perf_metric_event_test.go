package model

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/perf_metrics_setting"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// This is the released aggregate schema, before last_sample_at existed.
type legacyPerfMetric struct {
	Id             int    `gorm:"primaryKey"`
	ModelName      string `gorm:"size:128;uniqueIndex:idx_perf_model_group_bucket,priority:1"`
	Group          string `gorm:"column:group;size:64;uniqueIndex:idx_perf_model_group_bucket,priority:2"`
	BucketTs       int64  `gorm:"uniqueIndex:idx_perf_model_group_bucket,priority:3;index:idx_perf_bucket_ts"`
	RequestCount   int64  `gorm:"default:0"`
	SuccessCount   int64  `gorm:"default:0"`
	TotalLatencyMs int64  `gorm:"default:0"`
	TtftSumMs      int64  `gorm:"default:0"`
	TtftCount      int64  `gorm:"default:0"`
	OutputTokens   int64  `gorm:"default:0"`
	GenerationMs   int64  `gorm:"default:0"`
}

func (legacyPerfMetric) TableName() string { return "perf_metrics" }

func TestPerfMetricEventDatabaseMatrix(t *testing.T) {
	for _, dialect := range []struct{ name, env string }{{"sqlite", ""}, {"mysql", "TEST_PERF_MYSQL_DSN"}, {"postgres", "TEST_PERF_POSTGRES_DSN"}} {
		t.Run(dialect.name, func(t *testing.T) {
			var driver gorm.Dialector
			switch dialect.name {
			case "sqlite":
				driver = sqlite.Open(filepath.Join(t.TempDir(), "events.db"))
			case "mysql":
				if os.Getenv(dialect.env) == "" {
					t.Skip("set " + dialect.env + " to run this database")
				}
				driver = mysql.Open(os.Getenv(dialect.env))
			case "postgres":
				if os.Getenv(dialect.env) == "" {
					t.Skip("set " + dialect.env + " to run this database")
				}
				driver = postgresMigrationDialector{postgres.Dialector{Config: &postgres.Config{DSN: os.Getenv(dialect.env), PreferSimpleProtocol: true}}}
			}
			db, err := gorm.Open(driver, newGormConfig(dialect.name == "sqlite"))
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			sqlDB.SetMaxOpenConns(1)
			previousDB, previousLog, previousType := DB, LOG_DB, common.MainDatabaseType()
			DB, LOG_DB = db, db
			common.SetMainDatabaseType(common.DatabaseType(dialect.name))
			initCol()
			common.OptionMapRWMutex.Lock()
			previousOptions := maps.Clone(common.OptionMap)
			common.OptionMap = make(map[string]string)
			common.OptionMapRWMutex.Unlock()
			previousSettings := perf_metrics_setting.GetSetting()
			config.UpdateConfigFromMap(config.GlobalConfig.Get("perf_metrics_setting"), map[string]string{"enabled": "true", "retention_days": "0"})
			t.Cleanup(func() {
				require.NoError(t, db.Migrator().DropTable(&PerfMetricEvent{}, &PerfMetric{}, &PerfMetricCollector{}, &Option{}, &Task{}))
				DB, LOG_DB = previousDB, previousLog
				common.SetMainDatabaseType(previousType)
				initCol()
				common.OptionMapRWMutex.Lock()
				common.OptionMap = previousOptions
				common.OptionMapRWMutex.Unlock()
				config.UpdateConfigFromMap(config.GlobalConfig.Get("perf_metrics_setting"), map[string]string{"enabled": strconv.FormatBool(previousSettings.Enabled), "retention_days": strconv.Itoa(previousSettings.RetentionDays)})
				require.NoError(t, sqlDB.Close())
			})
			require.NoError(t, db.Migrator().DropTable(&PerfMetricEvent{}, &PerfMetric{}, &Option{}, &Task{}))
			query := "SELECT version()"
			if dialect.name == "sqlite" {
				query = "SELECT sqlite_version()"
			}
			var version string
			require.NoError(t, db.Raw(query).Scan(&version).Error)
			t.Logf("database version: %s", version)
			ctx := context.Background()
			now := time.Now().Unix()
			bucket := now - now%3600
			manual := `{"version":9,"companies":[{"id":"original-openai","name":"Openai","status":"active","slo":97,"owner":"manual-owner","note":"keep manual note","groups":[{"id":"manual-group","name":"Manual group","models":["legacy-model","private-alias"],"enabled":true,"slo":97,"min_samples":250}]}]}`
			reset := func(t *testing.T) {
				t.Helper()
				require.NoError(t, db.Migrator().DropTable(&PerfMetricEvent{}, &PerfMetric{}, &PerfMetricCollector{}, &Option{}, &Task{}))
				require.NoError(t, db.AutoMigrate(&PerfMetric{}, &PerfMetricEvent{}, &PerfMetricCollector{}, &Option{}, &Task{}))
			}

			t.Run("fresh schema is idempotent and keeps deduplication constraints", func(t *testing.T) {
				reset(t)
				for range 2 {
					require.NoError(t, db.AutoMigrate(&PerfMetric{}, &PerfMetricEvent{}, &PerfMetricCollector{}, &Option{}, &Task{}))
				}
				assert.True(t, db.Migrator().HasIndex(&PerfMetric{}, "idx_perf_model_group_bucket"))
				assert.True(t, db.Migrator().HasIndex(&PerfMetricEvent{}, "idx_perf_event_projection"))
				status, err := GetPerfMetricCollectionStatus(ctx, db)
				require.NoError(t, err)
				assert.Equal(t, PerfMetricCollectionStatus{}, status)
				row := PerfMetric{ModelName: "constraint", Group: "default", BucketTs: bucket, RequestCount: 1}
				require.NoError(t, db.Create(&row).Error)
				row.Id = 0
				assert.Error(t, db.Create(&row).Error, "model/group/bucket uniqueness survives startup")
				require.NoError(t, db.Where("model_name = ?", "constraint").Delete(&PerfMetric{}).Error)
			})

			t.Run("released aggregates and manual configuration survive two upgrades", func(t *testing.T) {
				reset(t)
				require.NoError(t, db.Migrator().DropTable(&PerfMetricEvent{}, &PerfMetric{}))
				require.NoError(t, db.AutoMigrate(&legacyPerfMetric{}))
				require.NoError(t, db.Create(&legacyPerfMetric{ModelName: "legacy-model", Group: "retired-group", BucketTs: bucket, RequestCount: 17, SuccessCount: 13, TotalLatencyMs: 17000, OutputTokens: 400, GenerationMs: 20000}).Error)
				require.NoError(t, db.Create(&Option{Key: PerformanceHealthConfigOption, Value: manual}).Error)
				// The sole task schema addition in this stage is the pending flag.
				// Dropping it restores the prior released task schema and indexes.
				require.NoError(t, db.Migrator().DropColumn(&Task{}, "PerformanceSamplePending"))
				require.NoError(t, db.Migrator().DropColumn(&Task{}, "PerformanceSampleError"))
				require.NoError(t, db.Table("tasks").Create(map[string]any{"id": 41, "task_id": "legacy-task", "status": TaskStatusSuccess, "group": "retired-group", "submit_time": now - 120, "finish_time": now}).Error)
				for range 2 {
					require.NoError(t, db.AutoMigrate(&PerfMetric{}, &PerfMetricEvent{}, &PerfMetricCollector{}, &Task{}))
				}
				var metric PerfMetric
				require.NoError(t, db.Where("model_name = ?", "legacy-model").First(&metric).Error)
				assert.Equal(t, int64(17), metric.RequestCount)
				assert.Equal(t, int64(13), metric.SuccessCount)
				assert.Zero(t, metric.LastSampleAt, "old buckets do not acquire a fabricated sample watermark")
				var option Option
				require.NoError(t, db.Where(map[string]any{"key": PerformanceHealthConfigOption}).First(&option).Error)
				assert.Equal(t, manual, option.Value)
				var task Task
				require.NoError(t, db.First(&task, 41).Error)
				assert.Equal(t, "legacy-task", task.TaskID)
				assert.False(t, task.PerformanceSamplePending, "old terminal tasks do not fabricate new history")
				assert.Empty(t, task.PerformanceSampleError)
				var eventCount int64
				require.NoError(t, db.Model(&PerfMetricEvent{}).Count(&eventCount).Error)
				assert.Zero(t, eventCount)
			})

			t.Run("acceptance replay and projection preserve exact counters", func(t *testing.T) {
				reset(t)
				require.NoError(t, db.Create(&PerfMetric{ModelName: "legacy-model", Group: "retired-group", BucketTs: bucket, RequestCount: 17, SuccessCount: 13, TotalLatencyMs: 17000, OutputTokens: 400, GenerationMs: 20000}).Error)
				events := []PerfMetricEvent{
					{ID: "request:one", ModelName: "legacy-model", Group: "retired-group", BucketTs: bucket, SampleAt: now, AcceptedAt: now + 1, Success: true, LatencyMs: 1000, HasTtft: true, TtftMs: 100, OutputTokensKnown: true, OutputTokens: 200, GenerationMs: 10000},
					{ID: "request:two", ModelName: "legacy-model", Group: "retired-group", BucketTs: bucket, SampleAt: now, AcceptedAt: now + 2, LatencyMs: 2000},
					{ID: "request:zero", ModelName: "legacy-model", Group: "retired-group", BucketTs: bucket, SampleAt: now, AcceptedAt: now + 3, Success: true, OutputTokensKnown: true, GenerationMs: 10000},
					{ID: "request:unknown", ModelName: "legacy-model", Group: "retired-group", BucketTs: bucket, SampleAt: now, AcceptedAt: now + 4, Success: true, OutputTokens: 9000, GenerationMs: 1000},
				}
				for i := range events {
					require.NoError(t, AcceptPerfMetricEvent(ctx, db, &events[i]))
				}
				replay := events[0]
				replay.Success = false
				require.NoError(t, AcceptPerfMetricEvent(ctx, db, &replay))
				status, err := GetPerfMetricCollectionStatus(ctx, db)
				require.NoError(t, err)
				assert.Equal(t, int64(4), status.PendingEvents)
				assert.Equal(t, now+1, status.OldestPendingAt)
				assert.Equal(t, now, status.OldestPendingSampleAt)
				assert.Equal(t, now+4, status.LastAcceptedAt)
				assert.Equal(t, now, status.LastSampleAt, "durably accepted samples expose their real terminal time before projection")
				assert.Zero(t, status.LastProjectedAt)
				count, err := ProjectPerfMetricEvents(ctx, db)
				require.NoError(t, err)
				assert.Equal(t, 4, count)
				count, err = ProjectPerfMetricEvents(ctx, db)
				require.NoError(t, err)
				assert.Zero(t, count)
				var metric PerfMetric
				require.NoError(t, db.Where("model_name = ?", "legacy-model").First(&metric).Error)
				assert.Equal(t, int64(21), metric.RequestCount)
				assert.Equal(t, int64(16), metric.SuccessCount)
				assert.Equal(t, int64(20000), metric.TotalLatencyMs)
				assert.Equal(t, int64(600), metric.OutputTokens)
				assert.Equal(t, int64(40000), metric.GenerationMs, "known zero contributes duration; unknown output does not")
				assert.Equal(t, int64(100), metric.TtftSumMs)
				assert.Equal(t, int64(1), metric.TtftCount)
				assert.Equal(t, now, metric.LastSampleAt)
				status, err = GetPerfMetricCollectionStatus(ctx, db)
				require.NoError(t, err)
				assert.Zero(t, status.PendingEvents)
				assert.Equal(t, now, status.LastSampleAt)
				assert.Positive(t, status.LastProjectedAt)
			})

			t.Run("transaction rollback leaves no accepted event", func(t *testing.T) {
				reset(t)
				forced := errors.New("caller transaction rejected")
				err := db.Transaction(func(tx *gorm.DB) error {
					require.NoError(t, AcceptPerfMetricEvent(ctx, tx, &PerfMetricEvent{ID: "transaction:rollback", ModelName: "new-model", BucketTs: bucket, SampleAt: now}))
					return forced
				})
				require.ErrorIs(t, err, forced)
				var count int64
				require.NoError(t, db.Model(&PerfMetricEvent{}).Where("id = ?", "transaction:rollback").Count(&count).Error)
				assert.Zero(t, count)
			})

			t.Run("collector updates clear clean exits but preserve crashed risk", func(t *testing.T) {
				reset(t)
				require.NoError(t, db.Create(&Task{TaskID: "pending-terminal", Status: TaskStatusSuccess, FinishTime: now - 90, PerformanceSamplePending: true, PerformanceSampleError: "invalid_dimensions"}).Error)
				status, err := GetPerfMetricCollectionStatus(ctx, db)
				require.NoError(t, err)
				assert.Equal(t, int64(1), status.PendingTaskEvents)
				assert.Equal(t, int64(1), status.FailedTaskEvents)
				assert.Equal(t, now-90, status.OldestPendingTaskAt)
				require.NoError(t, SavePerfMetricCollector(ctx, db, PerfMetricCollector{ID: "clean-boot", HeartbeatAt: now, UnpersistedEvents: 1, OldestUnpersistedAt: now - 10}))
				require.NoError(t, SavePerfMetricCollector(ctx, db, PerfMetricCollector{ID: "clean-boot", HeartbeatAt: now + 1, StoppedAt: now + 1}))
				require.NoError(t, SavePerfMetricCollector(ctx, db, PerfMetricCollector{ID: "crashed-boot", HeartbeatAt: now - 120, UnpersistedEvents: 1, OldestUnpersistedAt: now - 130, LastErrorAt: now - 120}))
				require.NoError(t, SavePerfMetricCollector(ctx, db, PerfMetricCollector{ID: "dropped-boot", HeartbeatAt: now, StoppedAt: now, DroppedEvents: 1}))
				collectors, err := GetPerfMetricCollectors(ctx, db)
				require.NoError(t, err)
				require.Len(t, collectors, 2)
				byID := make(map[string]PerfMetricCollector, len(collectors))
				for _, collector := range collectors {
					byID[collector.ID] = collector
				}
				assert.NotContains(t, byID, "clean-boot", "zero-valued updates clear previously pending state")
				assert.Equal(t, int64(1), byID["crashed-boot"].UnpersistedEvents)
				assert.Equal(t, now-130, byID["crashed-boot"].OldestUnpersistedAt)
				assert.Equal(t, int64(1), byID["dropped-boot"].DroppedEvents)
			})

			t.Run("task terminal markers survive failed staging and isolate invalid samples", func(t *testing.T) {
				reset(t)
				task := Task{TaskID: "task-terminal", Status: TaskStatusInProgress, Group: "retired-group", SubmitTime: now - 120, StartTime: now - 100, Quota: 123,
					Properties: Properties{OriginModelName: "task-model"}}
				require.NoError(t, task.Insert())
				zero := int64(0)
				task.PrivateData.PerformanceOutputTokens = &zero
				task.Status, task.FinishTime = TaskStatusSuccess, now
				won, err := task.UpdateWithStatus(TaskStatusInProgress)
				require.NoError(t, err)
				require.True(t, won)
				assert.True(t, task.PerformanceSamplePending)
				forced := errors.New("task metric outbox unavailable")
				const callback = "test:perf-event-reject-task-staging"
				require.NoError(t, db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
					if tx.Statement.Table == "perf_metric_events" {
						tx.AddError(forced)
					}
				}))
				count, err := StagePerfMetricTaskEvents(ctx, db)
				db.Callback().Create().Remove(callback)
				require.ErrorIs(t, err, forced)
				assert.Zero(t, count)
				var stored Task
				require.NoError(t, db.First(&stored, task.ID).Error)
				assert.Equal(t, TaskStatus(TaskStatusSuccess), stored.Status)
				assert.Equal(t, 123, stored.Quota, "telemetry write failure cannot alter task settlement")
				assert.True(t, stored.PerformanceSamplePending)
				count, err = StagePerfMetricTaskEvents(ctx, db)
				require.NoError(t, err)
				assert.Equal(t, 1, count)
				count, err = ProjectPerfMetricEvents(ctx, db)
				require.NoError(t, err)
				assert.Equal(t, 1, count)
				count, err = StagePerfMetricTaskEvents(ctx, db)
				require.NoError(t, err)
				assert.Zero(t, count)
				var metric PerfMetric
				require.NoError(t, db.Where("model_name = ?", "task-model").First(&metric).Error)
				assert.Equal(t, int64(1), metric.RequestCount)
				assert.Zero(t, metric.OutputTokens)
				assert.Equal(t, int64(100000), metric.GenerationMs, "explicit zero output retains its known duration")

				bad := Task{TaskID: "invalid-task", Status: TaskStatusSuccess, FinishTime: now, Properties: Properties{OriginModelName: strings.Repeat("a", 129)}}
				good := Task{TaskID: "healthy-task", Status: TaskStatusSuccess, FinishTime: now, Properties: Properties{OriginModelName: "healthy-model"}}
				require.NoError(t, bad.Insert())
				require.NoError(t, good.Insert())
				count, err = StagePerfMetricTaskEvents(ctx, db)
				require.ErrorContains(t, err, "invalid_dimensions")
				assert.Equal(t, 1, count, "one invalid task cannot block a healthy task in the same batch")
				require.NoError(t, db.First(&bad, bad.ID).Error)
				require.NoError(t, db.First(&good, good.ID).Error)
				assert.True(t, bad.PerformanceSamplePending)
				assert.Equal(t, "invalid_dimensions", bad.PerformanceSampleError)
				assert.False(t, good.PerformanceSamplePending)
				status, err := GetPerfMetricCollectionStatus(ctx, db)
				require.NoError(t, err)
				assert.Equal(t, int64(1), status.FailedTaskEvents)
				assert.Equal(t, int64(1), status.PendingTaskEvents)
			})

			t.Run("projection failure rolls back its claim and recovers once", func(t *testing.T) {
				reset(t)
				require.NoError(t, AcceptPerfMetricEvent(ctx, db, &PerfMetricEvent{ID: "request:retry", ModelName: "retry-model", BucketTs: bucket, SampleAt: now, Success: true, LatencyMs: 1500}))
				forced := errors.New("aggregate database write rejected")
				const callback = "test:perf-event-reject-aggregate"
				require.NoError(t, db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
					if tx.Statement.Table == "perf_metrics" {
						tx.AddError(forced)
					}
				}))
				count, err := ProjectPerfMetricEvents(ctx, db)
				db.Callback().Create().Remove(callback)
				require.ErrorIs(t, err, forced)
				assert.Zero(t, count)
				status, err := GetPerfMetricCollectionStatus(ctx, db)
				require.NoError(t, err)
				assert.Equal(t, int64(1), status.PendingEvents)
				var metricCount int64
				require.NoError(t, db.Model(&PerfMetric{}).Where("model_name = ?", "retry-model").Count(&metricCount).Error)
				assert.Zero(t, metricCount)
				count, err = ProjectPerfMetricEvents(ctx, db)
				require.NoError(t, err)
				assert.Equal(t, 1, count)
				count, err = ProjectPerfMetricEvents(ctx, db)
				require.NoError(t, err)
				assert.Zero(t, count)
			})

			t.Run("competing workers and replay count every event once", func(t *testing.T) {
				reset(t)
				event := PerfMetricEvent{ID: "request:concurrent", ModelName: "concurrent-model", BucketTs: bucket, SampleAt: now, Success: true}
				require.NoError(t, AcceptPerfMetricEvent(ctx, db, &event))
				start := make(chan struct{})
				results := make(chan error, 2)
				var workers sync.WaitGroup
				for range 2 {
					workers.Go(func() {
						<-start
						_, err := ProjectPerfMetricEvents(ctx, db)
						results <- err
					})
				}
				close(start)
				workers.Wait()
				close(results)
				for err := range results {
					require.NoError(t, err)
				}
				require.NoError(t, AcceptPerfMetricEvent(ctx, db, &event))
				count, err := ProjectPerfMetricEvents(ctx, db)
				require.NoError(t, err)
				assert.Zero(t, count)
				var metric PerfMetric
				require.NoError(t, db.Where("model_name = ?", "concurrent-model").First(&metric).Error)
				assert.Equal(t, int64(1), metric.RequestCount)
				assert.Equal(t, int64(1), metric.SuccessCount)
			})

			t.Run("real read-only database rejects acceptance and projection", func(t *testing.T) {
				reset(t)
				require.NoError(t, AcceptPerfMetricEvent(ctx, db, &PerfMetricEvent{ID: "request:readonly-pending", ModelName: "readonly-model", BucketTs: bucket, SampleAt: now}))
				readOnly, readWrite := "PRAGMA query_only = ON", "PRAGMA query_only = OFF"
				if dialect.name == "mysql" {
					readOnly, readWrite = "SET SESSION TRANSACTION READ ONLY", "SET SESSION TRANSACTION READ WRITE"
				}
				if dialect.name == "postgres" {
					readOnly, readWrite = "SET default_transaction_read_only = on", "SET default_transaction_read_only = off"
				}
				require.NoError(t, db.Exec(readOnly).Error)
				acceptErr := AcceptPerfMetricEvent(ctx, db, &PerfMetricEvent{ID: "request:readonly-rejected", ModelName: "readonly-model", BucketTs: bucket, SampleAt: now})
				_, projectionErr := ProjectPerfMetricEvents(ctx, db)
				require.NoError(t, db.Exec(readWrite).Error)
				assert.Error(t, acceptErr)
				assert.Error(t, projectionErr)
				var count int64
				require.NoError(t, db.Model(&PerfMetricEvent{}).Where("id = ?", "request:readonly-rejected").Count(&count).Error)
				assert.Zero(t, count)
				status, err := GetPerfMetricCollectionStatus(ctx, db)
				require.NoError(t, err)
				assert.Equal(t, int64(1), status.PendingEvents)
				countProjected, err := ProjectPerfMetricEvents(ctx, db)
				require.NoError(t, err)
				assert.Equal(t, 1, countProjected)
			})

			t.Run("retention keeps pending evidence and does not resurrect expired replays", func(t *testing.T) {
				reset(t)
				old := now - 3*86400
				require.NoError(t, AcceptPerfMetricEvent(ctx, db, &PerfMetricEvent{ID: "request:old-pending", ModelName: "old-model", BucketTs: old - old%3600, SampleAt: old}))
				require.NoError(t, DeletePerfMetricEventsBefore(ctx, db, now+10))
				var pending PerfMetricEvent
				require.NoError(t, db.First(&pending, "id = ?", "request:old-pending").Error)
				assert.Zero(t, pending.ProjectedAt)
				config.UpdateConfigFromMap(config.GlobalConfig.Get("perf_metrics_setting"), map[string]string{"retention_days": "1"})
				defer config.UpdateConfigFromMap(config.GlobalConfig.Get("perf_metrics_setting"), map[string]string{"retention_days": "0"})
				require.NoError(t, AcceptPerfMetricEvent(ctx, db, &PerfMetricEvent{ID: "request:expired-replay", ModelName: "old-model", BucketTs: old - old%3600, SampleAt: old}))
				var count int64
				require.NoError(t, db.Model(&PerfMetricEvent{}).Where("id = ?", "request:expired-replay").Count(&count).Error)
				assert.Zero(t, count)
			})
		})
	}
}
