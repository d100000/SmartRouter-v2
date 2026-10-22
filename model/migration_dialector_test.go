package model

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// TestStartupUpgradePreservesData runs against disposable databases only. Seed
// them by running this test in the source revision with TEST_STARTUP_PHASE=seed,
// then run the destination revision with phase=upgrade and the same snapshot.
// A fresh run creates its fixtures after the first real startup migration.
func TestStartupUpgradePreservesData(t *testing.T) {
	mainDSN := os.Getenv("TEST_STARTUP_MAIN_DSN")
	if mainDSN == "" {
		t.Skip("disposable startup database is not configured")
	}
	logDSN := os.Getenv("TEST_STARTUP_LOG_DSN")
	require.NotEmpty(t, logDSN, "exercise a separate log database")
	phase := os.Getenv("TEST_STARTUP_PHASE")
	require.Contains(t, []string{"seed", "upgrade", "fresh"}, phase)
	snapshotPath := os.Getenv("TEST_STARTUP_SNAPSHOT")
	require.NotEmpty(t, snapshotPath)
	oldDB, oldLogDB, oldMaster, oldSQLite := DB, LOG_DB, common.IsMasterNode, common.SQLitePath
	oldMainType, oldLogType := common.MainDatabaseType(), common.LogDatabaseType()
	t.Cleanup(func() {
		DB, LOG_DB, common.IsMasterNode, common.SQLitePath = oldDB, oldLogDB, oldMaster, oldSQLite
		common.SetDatabaseTypes(oldMainType, oldLogType)
		initCol()
	})
	common.IsMasterNode = true
	t.Setenv("SQL_DSN", mainDSN)
	t.Setenv("LOG_SQL_DSN", logDSN)
	t.Setenv("SQL_MAX_IDLE_CONNS", "1")
	t.Setenv("SQL_MAX_OPEN_CONNS", "2")
	type tableSnapshot struct {
		Rows    []map[string]string
		Indexes []string
	}
	expected := map[string]tableSnapshot{}
	if phase == "upgrade" {
		data, err := os.ReadFile(snapshotPath)
		require.NoError(t, err)
		require.NoError(t, common.Unmarshal(data, &expected))
	}
	for restart := range 2 {
		common.SQLitePath = os.Getenv("TEST_STARTUP_SQLITE_MAIN")
		require.NoError(t, InitDB())
		common.SQLitePath = os.Getenv("TEST_STARTUP_SQLITE_LOG")
		require.NoError(t, InitLogDB())
		mainSQL, err := DB.DB()
		require.NoError(t, err)
		logSQL, err := LOG_DB.DB()
		require.NoError(t, err)
		if restart == 0 && phase != "upgrade" {
			for _, fixture := range []struct {
				table string
				row   map[string]any
			}{
				{"users", map[string]any{"id": 87001, "username": "startup-upgrade-user", "password": "unusable-upgrade-fixture", "quota": int64(123456789012), "used_quota": 7654321, "status": 1}},
				{"tokens", map[string]any{"id": 87002, "user_id": 87001, "key": "startup-upgrade-fixture-token", "name": "upgrade token", "remain_quota": 9876543}},
				{"channels", map[string]any{"id": 87003, "type": 1, "name": "startup-upgrade-channel", "key": "startup-upgrade-fixture-key", "status": 1, "models": "upgrade-model", "group": "default", "used_quota": 654321}},
				{"options", map[string]any{"key": "billing_setting.billing_expr", "value": `{"upgrade-model":"tier(\"base\", p * 2 + cc * 2.5)"}`}},
				{"tasks", map[string]any{"id": 87004, "task_id": "startup-upgrade-task", "user_id": 87001, "channel_id": 87003, "quota": 123000, "status": "SUCCESS", "data": `{"result":"preserved"}`}},
				{"quota_data", map[string]any{"id": 87005, "user_id": 87001, "username": "startup-upgrade-user", "model_name": "upgrade-model", "created_at": 1790000000, "token_used": 500, "quota": 600, "count": 1}},
				{"overview_attempt_events", map[string]any{"id": "startup-upgrade-attempt", "request_id": "startup-upgrade-request", "status": "completed", "day": "2026-10-20", "channel_id": 87003, "base_quota": 1000.25, "quota_per_unit": 500000, "revenue_quota": 600, "cost_quota": 250.0625, "cost_known": true, "revenue_confirmed": true}},
				{"overview_dailies", map[string]any{"id": "startup-upgrade-daily", "day": "2026-10-20", "channel_id": 87003, "quota_per_unit": 500000, "revenue": 600, "cost": 250.0625, "complete_revenue": 600, "complete_cost": 250.0625, "requests": 1, "attempts": 1}},
			} {
				if DB.Migrator().HasTable(fixture.table) {
					require.NoError(t, DB.Table(fixture.table).Create(fixture.row).Error, fixture.table)
				}
			}
			require.NoError(t, LOG_DB.Table("logs").Create(map[string]any{"id": 87006, "user_id": 87001, "type": 2, "quota": 600, "content": "historical usage", "other": `{"cache_tokens":123,"cache_write_tokens":45}`}).Error)
			require.NoError(t, LOG_DB.Table("audit_logs").Create(map[string]any{"id": 87007, "event_id": "startup-upgrade-audit", "user_id": 87001, "created_at": 1790000000, "action": "channel.update", "content": "historical audit"}).Error)
			// Newly seeded channels and users need the same initialization that
			// an ordinary creation path or the following startup performs.
			require.NoError(t, migrateDB())
		}
		actual := map[string]tableSnapshot{}
		for _, table := range []string{"users", "tokens", "channels", "options", "tasks", "quota_data", "overview_attempt_events", "overview_dailies", "overview_collection_states", "upstream_suppliers", "upstream_credentials", "upstream_credential_versions", "channel_credential_bindings", "upstream_credential_fingerprint_secrets", "logs", "audit_logs"} {
			db := DB
			if table == "logs" || table == "audit_logs" {
				db = LOG_DB
			}
			if !db.Migrator().HasTable(table) {
				continue
			}
			query := db.Table(table).Order("id")
			if table == "options" {
				query = db.Table(table).Where(map[string]any{"key": "billing_setting.billing_expr"})
			}
			var rows []map[string]any
			require.NoError(t, query.Find(&rows).Error, table)
			snapshot := tableSnapshot{}
			for _, row := range rows {
				values := map[string]string{}
				for key, value := range row {
					if bytes, ok := value.([]byte); ok {
						value = string(bytes)
					}
					values[key] = fmt.Sprint(value)
				}
				snapshot.Rows = append(snapshot.Rows, values)
			}
			indexes, err := db.Migrator().GetIndexes(table)
			require.NoError(t, err, table)
			for _, index := range indexes {
				snapshot.Indexes = append(snapshot.Indexes, index.Name())
			}
			actual[table] = snapshot
		}
		for table, before := range expected {
			after, ok := actual[table]
			require.True(t, ok, "table %s must survive", table)
			require.Len(t, after.Rows, len(before.Rows), table)
			for index, row := range before.Rows {
				for column, value := range row {
					assert.Equal(t, value, after.Rows[index][column], "%s.%s on restart %d", table, column, restart)
				}
			}
			for _, index := range before.Indexes {
				assert.Contains(t, after.Indexes, index, table)
			}
		}
		if restart == 1 {
			// Both uniqueness guarantees must still be enforced by the engine.
			assert.Error(t, DB.Table("tokens").Create(map[string]any{"id": 87999, "key": "startup-upgrade-fixture-token"}).Error)
			assert.Error(t, LOG_DB.Table("audit_logs").Create(map[string]any{"id": 87999, "event_id": "startup-upgrade-audit"}).Error)
			if phase == "seed" {
				data, err := common.Marshal(actual)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(snapshotPath, data, 0600))
			} else {
				assert.True(t, DB.Migrator().HasColumn("quota_data", "cache_read_tokens"))
				assert.True(t, DB.Migrator().HasColumn("quota_data", "cache_creation_tokens"))
				assert.True(t, LOG_DB.Migrator().HasIndex("audit_logs", "idx_audit_time_event"))
				for _, field := range []string{"cache_read_tokens", "cache_creation_tokens"} {
					var quantity int64
					require.NoError(t, DB.Table("quota_data").Select(field).Where("id = ?", 87005).Scan(&quantity).Error)
					assert.Zero(t, quantity, "historical cache usage is not backfilled")
				}
				recorder := &migrationSQLRecorder{}
				DB, LOG_DB = DB.Session(&gorm.Session{Logger: recorder}), LOG_DB.Session(&gorm.Session{Logger: recorder})
				require.NoError(t, migrateDB())
				require.NoError(t, migrateLOGDB())
				assert.Empty(t, recorder.schemaMutations(), "repeated startup must not issue schema changes")
			}
		}
		if phase == "fresh" {
			expected = actual
		}
		require.NoError(t, logSQL.Close())
		require.NoError(t, mainSQL.Close())
	}
}

type MigrationIdentityFields struct {
	ID        int    `gorm:"primaryKey"`
	Name      string `gorm:"size:64;unique"`
	Reference string `gorm:"size:64;uniqueIndex"`
	Provider  string `gorm:"size:32;uniqueIndex:,composite:provider_subject"`
	Subject   string `gorm:"size:64;uniqueIndex:,composite:provider_subject"`
}

type migrationIdentityV1 struct {
	MigrationIdentityFields
	Digest string `gorm:"type:char(32)"`
}

type migrationIdentityV2 struct {
	MigrationIdentityFields
	Digest string `gorm:"type:char(64)"`
	Note   string `gorm:"size:128"`
}

type migrationConstraintV1 struct {
	ID   int    `gorm:"primaryKey"`
	Name string `gorm:"size:64"`
}

type migrationConstraintV2 struct {
	ID   int    `gorm:"primaryKey"`
	Name string `gorm:"size:64;unique"`
}

type migrationDecimalV1 struct {
	ID    int     `gorm:"primaryKey"`
	Price float64 `gorm:"type:decimal(10,6);default:0"`
}

type migrationDecimalV2 struct {
	ID    int     `gorm:"primaryKey"`
	Price float64 `gorm:"type:decimal(12,6);not null;default:0"`
}

type migrationDecimalV3 struct {
	ID    int     `gorm:"primaryKey"`
	Price float64 `gorm:"type:decimal(12,6);not null;default:1.25"`
}

func TestMigrationSchemaStability(t *testing.T) {
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			var dsn string
			switch dialect {
			case "sqlite":
				dsn = "local"
				previousPath := common.SQLitePath
				common.SQLitePath = filepath.Join(t.TempDir(), "migration.db")
				t.Cleanup(func() { common.SQLitePath = previousPath })
			case "mysql":
				dsn = os.Getenv("TEST_MYSQL_DSN")
			case "postgres":
				dsn = os.Getenv("TEST_POSTGRES_DSN")
			}
			if dsn == "" {
				t.Skip("test database DSN is not configured")
			}
			t.Setenv("MIGRATION_TEST_DSN", dsn)
			db, _, err := chooseDB("MIGRATION_TEST_DSN", false)
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			t.Cleanup(func() { _ = sqlDB.Close() })
			recorder := &migrationSQLRecorder{}
			db = db.Session(&gorm.Session{Logger: recorder})

			t.Run("identity_and_indexes", func(t *testing.T) {
				const table = "migration_identity_test"
				t.Cleanup(func() { _ = db.Migrator().DropTable(table) })
				require.NoError(t, db.Table(table).AutoMigrate(&migrationIdentityV1{}))
				row := migrationIdentityV1{
					MigrationIdentityFields: MigrationIdentityFields{ID: 1, Name: "root", Reference: "token-reference", Provider: "oidc", Subject: "subject"},
					Digest:                  "old-digest",
				}
				require.NoError(t, db.Table(table).Create(&row).Error)
				recorder.reset()
				require.NoError(t, db.Table(table).AutoMigrate(&migrationIdentityV1{}))
				assert.Empty(t, recorder.schemaMutations())

				require.NoError(t, db.Table(table).AutoMigrate(&migrationIdentityV2{}))
				columns, err := db.Table(table).Migrator().ColumnTypes(&migrationIdentityV2{})
				require.NoError(t, err)
				for _, column := range columns {
					if column.Name() == "digest" {
						length, ok := column.Length()
						require.True(t, ok)
						assert.EqualValues(t, 64, length)
					}
				}
				assert.True(t, db.Table(table).Migrator().HasColumn(&migrationIdentityV2{}, "note"))
				recorder.reset()
				require.NoError(t, db.Table(table).AutoMigrate(&migrationIdentityV2{}))
				assert.Empty(t, recorder.schemaMutations())
				var saved migrationIdentityV2
				require.NoError(t, db.Table(table).First(&saved, 1).Error)
				assert.Equal(t, row.MigrationIdentityFields, saved.MigrationIdentityFields)
				expectedDigest := row.Digest
				if dialect == "postgres" {
					expectedDigest += strings.Repeat(" ", 64-len(row.Digest))
				}
				assert.Equal(t, expectedDigest, saved.Digest)
				for _, duplicate := range []migrationIdentityV2{
					{MigrationIdentityFields: MigrationIdentityFields{Name: "root", Reference: "other-1", Provider: "other", Subject: "1"}},
					{MigrationIdentityFields: MigrationIdentityFields{Name: "other-2", Reference: "token-reference", Provider: "other", Subject: "2"}},
					{MigrationIdentityFields: MigrationIdentityFields{Name: "other-3", Reference: "other-3", Provider: "oidc", Subject: "subject"}},
				} {
					assert.Error(t, db.Table(table).Create(&duplicate).Error)
				}
			})

			t.Run("unique_constraint_changes", func(t *testing.T) {
				const table = "migration_constraint_test"
				t.Cleanup(func() { _ = db.Migrator().DropTable(table) })
				require.NoError(t, db.Table(table).AutoMigrate(&migrationConstraintV1{}))
				require.NoError(t, db.Table(table).Create(&migrationConstraintV1{Name: "existing"}).Error)
				require.NoError(t, db.Table(table).AutoMigrate(&migrationConstraintV2{}))
				assert.Error(t, db.Table(table).Create(&migrationConstraintV2{Name: "existing"}).Error)
				recorder.reset()
				require.NoError(t, db.Table(table).AutoMigrate(&migrationConstraintV2{}))
				assert.Empty(t, recorder.schemaMutations())
				require.NoError(t, db.Table(table).AutoMigrate(&migrationConstraintV1{}))
				require.NoError(t, db.Table(table).Create(&migrationConstraintV1{Name: "existing"}).Error)
			})

			if dialect == "postgres" {
				t.Run("renamed_unique_constraints", func(t *testing.T) {
					const table = "migration_renamed_unique"
					t.Cleanup(func() { _ = db.Migrator().DropTable(table) })
					tableDB := db.Table(table).Session(&gorm.Session{})
					require.NoError(t, tableDB.AutoMigrate(&migrationConstraintV1{}))
					original := migrationConstraintV1{Name: "existing"}
					require.NoError(t, tableDB.Create(&original).Error)
					for _, name := range []string{"models_model_name_key", `imported "model" name`} {
						require.NoError(t, db.Exec("ALTER TABLE ? ADD CONSTRAINT ? UNIQUE (name)", clause.Table{Name: table}, clause.Column{Name: name}).Error)
					}
					require.NoError(t, db.Exec("ALTER TABLE ? ADD CONSTRAINT keep_composite UNIQUE (id, name)", clause.Table{Name: table}).Error)
					require.NoError(t, db.Exec("CREATE INDEX keep_name_lookup ON ? (name)", clause.Table{Name: table}).Error)
					// A unique field must keep the old names and still reject duplicates.
					require.NoError(t, tableDB.AutoMigrate(&migrationConstraintV2{}))
					require.Error(t, tableDB.Create(&migrationConstraintV2{Name: "existing"}).Error)
					require.NoError(t, db.Exec("CREATE TABLE migration_unique_reference (name varchar(64) REFERENCES migration_renamed_unique(name))").Error)
					t.Cleanup(func() { _ = db.Migrator().DropTable("migration_unique_reference") })
					require.NoError(t, db.Exec("INSERT INTO migration_unique_reference (name) VALUES (?)", "existing").Error)
					require.Error(t, tableDB.AutoMigrate(&migrationConstraintV1{}), "dependent foreign keys must not be cascaded away")
					for _, name := range []string{"models_model_name_key", `imported "model" name`} {
						assert.True(t, tableDB.Migrator().HasConstraint(&migrationConstraintV1{}, name), "all old constraints must survive rollback")
					}
					var references int64
					require.NoError(t, db.Table("migration_unique_reference").Count(&references).Error)
					assert.EqualValues(t, 1, references)
					require.NoError(t, db.Migrator().DropTable("migration_unique_reference"))
					// Removing column uniqueness must resolve both actual constraint names.
					require.NoError(t, tableDB.AutoMigrate(&migrationConstraintV1{}))
					recorder.reset()
					require.NoError(t, tableDB.AutoMigrate(&migrationConstraintV1{}))
					assert.Empty(t, recorder.schemaMutations())
					require.NoError(t, tableDB.Create(&migrationConstraintV1{Name: "existing"}).Error)
					assert.True(t, tableDB.Migrator().HasConstraint(&migrationConstraintV1{}, "keep_composite"))
					assert.True(t, tableDB.Migrator().HasIndex(&migrationConstraintV1{}, "keep_name_lookup"))
					var rows []migrationConstraintV1
					require.NoError(t, tableDB.Order("id").Find(&rows).Error)
					require.Len(t, rows, 2)
					assert.Equal(t, original, rows[0])
				})
			}

			if dialect == "mysql" {
				t.Run("decimal_default_and_real_changes", func(t *testing.T) {
					const table = "migration_decimal_test"
					t.Cleanup(func() { _ = db.Migrator().DropTable(table) })
					require.NoError(t, db.Table(table).AutoMigrate(&migrationDecimalV1{}))
					require.NoError(t, db.Table(table).Create(&migrationDecimalV1{ID: 1, Price: 12.345678}).Error)
					for _, target := range []any{&migrationDecimalV1{}, &migrationDecimalV2{}, &migrationDecimalV3{}} {
						require.NoError(t, db.Table(table).AutoMigrate(target))
						recorder.reset()
						require.NoError(t, db.Table(table).AutoMigrate(target))
						assert.Empty(t, recorder.schemaMutations())
					}
					columns, err := db.Table(table).Migrator().ColumnTypes(&migrationDecimalV3{})
					require.NoError(t, err)
					for _, column := range columns {
						if column.Name() == "price" {
							precision, scale, ok := column.DecimalSize()
							require.True(t, ok)
							assert.EqualValues(t, 12, precision)
							assert.EqualValues(t, 6, scale)
							nullable, ok := column.Nullable()
							require.True(t, ok)
							assert.False(t, nullable)
						}
					}
					require.NoError(t, db.Table(table).Create(&map[string]any{"id": 2}).Error)
					var prices []float64
					require.NoError(t, db.Table(table).Order("id").Pluck("price", &prices).Error)
					assert.Equal(t, []float64{12.345678, 1.25}, prices)
				})
			}
		})
	}
}
