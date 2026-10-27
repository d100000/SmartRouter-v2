package model

import (
	"database/sql"
	"errors"
	"maps"
	"os"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"
)

func setPerformanceHealthConfigFixture(t *testing.T, db *gorm.DB, config PerformanceHealthConfig) {
	t.Helper()
	encoded, err := common.Marshal(config)
	require.NoError(t, err)
	require.NoError(t, db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "key"}}, DoUpdates: clause.Assignments(map[string]any{"value": string(encoded)})}).Create(&Option{Key: PerformanceHealthConfigOption, Value: string(encoded)}).Error)
}

func TestValidatePerformanceHealthConfigRejectsOverlappingModels(t *testing.T) {
	config := PerformanceHealthConfig{Companies: []PerformanceCompany{
		{ID: "openai", Name: "OpenAI", Groups: []PerformanceModelGroup{{ID: "general", Name: "General", Models: []string{"gpt-4.1"}}}},
		{ID: "backup", Name: "Backup", Groups: []PerformanceModelGroup{{ID: "fallback", Name: "Fallback", Models: []string{"gpt-4.1"}}}},
	}}

	err := validatePerformanceHealthConfig(config)
	require.ErrorContains(t, err, "assigned to both")
}

func TestPerformanceHealthConfigDatabaseMatrix(t *testing.T) {
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			var driver gorm.Dialector
			switch dialect {
			case "sqlite":
				driver = sqlite.Open(":memory:")
			case "mysql":
				if os.Getenv("TEST_MYSQL_DSN") == "" {
					t.Skip("TEST_MYSQL_DSN is not configured")
				}
				driver = mysql.Open(os.Getenv("TEST_MYSQL_DSN"))
			case "postgres":
				if os.Getenv("TEST_POSTGRES_DSN") == "" {
					t.Skip("TEST_POSTGRES_DSN is not configured")
				}
				driver = postgres.Open(os.Getenv("TEST_POSTGRES_DSN"))
			}
			db, err := gorm.Open(driver, &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: "performance_health_test_"}})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			sqlDB.SetMaxOpenConns(1)
			previousDB, previousType := DB, common.MainDatabaseType()
			common.OptionMapRWMutex.Lock()
			previousOptions := maps.Clone(common.OptionMap)
			common.OptionMap = make(map[string]string)
			common.OptionMapRWMutex.Unlock()
			DB = db
			common.SetMainDatabaseType(common.DatabaseType(dialect))
			initCol()
			t.Cleanup(func() {
				require.NoError(t, db.Migrator().DropTable(&Option{}))
				DB = previousDB
				common.SetMainDatabaseType(previousType)
				initCol()
				common.OptionMapRWMutex.Lock()
				common.OptionMap = previousOptions
				common.OptionMapRWMutex.Unlock()
				require.NoError(t, sqlDB.Close())
			})
			require.NoError(t, db.AutoMigrate(&Option{}))
			versionQuery := "SELECT version()"
			if dialect == "sqlite" {
				versionQuery = "SELECT sqlite_version()"
			}
			var version string
			require.NoError(t, db.Raw(versionQuery).Scan(&version).Error)
			t.Logf("database version: %s", version)

			t.Run("initialization is nonempty and idempotent", func(t *testing.T) {
				require.NoError(t, db.Where(map[string]any{"key": PerformanceHealthConfigOption}).Delete(&Option{}).Error)
				before, err := GetPerformanceHealthConfig()
				require.NoError(t, err)
				assert.Empty(t, before.Companies, "GET does not initialize presets")
				var count int64
				require.NoError(t, db.Model(&Option{}).Count(&count).Error)
				assert.Zero(t, count)
				require.NoError(t, InitPerformanceHealthConfig())
				first, err := GetPerformanceHealthConfig()
				require.NoError(t, err)
				require.Len(t, first.Companies, 3)
				for _, company := range first.Companies {
					require.NotEmpty(t, company.Groups)
					assert.NotEmpty(t, company.Groups[0].Models)
					assert.Equal(t, int64(100), company.Groups[0].MinSamples)
					require.NotNil(t, company.SLOConfirmed)
					assert.False(t, *company.SLOConfirmed)
				}
				require.NoError(t, InitPerformanceHealthConfig())
				second, err := GetPerformanceHealthConfig()
				require.NoError(t, err)
				assert.Equal(t, first, second)
			})

			legacy := PerformanceHealthConfig{Version: 7, Companies: []PerformanceCompany{
				{ID: "original-openai-id", Name: " OpenAI ", Status: "active", SLO: 97, Note: "keep my note", Groups: []PerformanceModelGroup{}},
				{ID: "manual", Name: "Manual", Status: "active", SLO: 96, Groups: []PerformanceModelGroup{{ID: "manual-group", Name: "Manual group", Enabled: true, SLO: 96, MinSamples: 10, Models: []string{"gpt-4.1", "private-model"}}}},
			}}
			t.Run("existing config survives startup and preview only fills gaps", func(t *testing.T) {
				setPerformanceHealthConfigFixture(t, db, legacy)
				require.NoError(t, InitPerformanceHealthConfig())
				require.NoError(t, InitPerformanceHealthConfig())
				current, err := GetPerformanceHealthConfig()
				require.NoError(t, err)
				assert.Equal(t, int64(7), current.Version)
				assert.Len(t, current.Companies, 2)
				preview, err := PreviewPerformanceHealthPresets()
				require.NoError(t, err)
				assert.Equal(t, int64(7), preview.ExpectedConfigVersion)
				assert.NotEmpty(t, preview.PreviewToken)
				var existing *PerformanceCompany
				for i := range preview.Config.Companies {
					company := &preview.Config.Companies[i]
					if company.ID == "original-openai-id" {
						existing = company
					}
				}
				require.NotNil(t, existing)
				assert.Equal(t, "keep my note", existing.Note)
				assert.Equal(t, float64(97), existing.SLO)
				require.NotEmpty(t, existing.Groups)
				assert.Equal(t, float64(97), existing.Groups[0].SLO)
				assert.NotContains(t, existing.Groups[0].Models, "gpt-4.1", "manual assignment has priority")
				assert.NotEmpty(t, preview.Conflicts)
				saved, err := ApplyPerformanceHealthPresets(preview.ExpectedConfigVersion, preview.PreviewToken)
				require.NoError(t, err)
				assert.Equal(t, int64(8), saved.Version)
				preview, err = PreviewPerformanceHealthPresets()
				require.NoError(t, err)
				assert.Empty(t, preview.AddedCompanies)
				assert.Empty(t, preview.AddedGroups)
				assert.Empty(t, preview.AddedModels)
				again, err := ApplyPerformanceHealthPresets(preview.ExpectedConfigVersion, preview.PreviewToken)
				require.NoError(t, err)
				assert.Equal(t, saved.Version, again.Version)
			})

			t.Run("stale writes and previews cannot overwrite current data", func(t *testing.T) {
				setPerformanceHealthConfigFixture(t, db, legacy)
				old, err := GetPerformanceHealthConfig()
				require.NoError(t, err)
				preview, err := PreviewPerformanceHealthPresets()
				require.NoError(t, err)
				old.Companies[0].Note = "new note"
				saved, err := SavePerformanceHealthConfig(old)
				require.NoError(t, err)
				_, err = SavePerformanceHealthConfig(old)
				assert.ErrorIs(t, err, ErrPerformanceHealthConflict)
				_, err = ApplyPerformanceHealthPresets(preview.ExpectedConfigVersion, preview.PreviewToken)
				assert.ErrorIs(t, err, ErrPerformanceHealthConflict)
				other, err := common.Marshal(PerformanceHealthConfig{Version: saved.Version + 1, Companies: saved.Companies})
				require.NoError(t, err)
				require.NoError(t, db.Model(&Option{}).Where(commonKeyCol+" = ?", PerformanceHealthConfigOption).Update("value", string(other)).Error)
				read, err := GetPerformanceHealthConfig()
				require.NoError(t, err)
				assert.Equal(t, saved.Version+1, read.Version, "database is authoritative across instances")
			})

			t.Run("legacy saves preserve preset confirmation while allowing explicit confirmation", func(t *testing.T) {
				fixture := GetPerformanceHealthPresets()
				fixture.Companies[0].Owner, fixture.Companies[0].Note = "Previous owner", "Previous note"
				fixture.Companies = append(fixture.Companies,
					PerformanceCompany{ID: "manual-slo", Name: "Manual SLO", SLO: 97},
					PerformanceCompany{ID: "empty-models", Name: "Empty models", Groups: []PerformanceModelGroup{{ID: "empty-models-group", Name: "Empty models"}}},
				)
				setPerformanceHealthConfigFixture(t, db, fixture)
				current, err := GetPerformanceHealthConfig()
				require.NoError(t, err)
				assert.NotNil(t, current.Companies[3].Groups, "legacy null groups are returned as an empty array")
				assert.NotNil(t, current.Companies[4].Groups[0].Models, "legacy null models are returned as an empty array")
				oldClient, err := clonePerformanceHealthConfig(current)
				require.NoError(t, err)
				for i := range oldClient.Companies {
					company := &oldClient.Companies[i]
					company.PresetKey, company.SLOConfirmed = "", nil
					for j := range company.Groups {
						company.Groups[j].PresetKey, company.Groups[j].SLOConfirmed = "", nil
					}
				}
				oldClient.Companies[0].Owner, oldClient.Companies[0].Note = "", ""
				saved, err := SavePerformanceHealthConfig(oldClient)
				require.NoError(t, err)
				for i := range 3 {
					company := saved.Companies[i]
					assert.Equal(t, current.Companies[i].PresetKey, company.PresetKey)
					require.NotNil(t, company.SLOConfirmed)
					assert.False(t, *company.SLOConfirmed, "omitted metadata must not activate the template SLO")
					assert.Equal(t, current.Companies[i].Groups[0].PresetKey, company.Groups[0].PresetKey)
					require.NotNil(t, company.Groups[0].SLOConfirmed)
					assert.False(t, *company.Groups[0].SLOConfirmed)
				}
				assert.Empty(t, saved.Companies[0].Owner)
				assert.Empty(t, saved.Companies[0].Note, "a full save can explicitly clear notes")
				assert.Equal(t, float64(97), saved.Companies[3].SLO)
				assert.Nil(t, saved.Companies[3].SLOConfirmed, "legacy manual targets remain confirmed by compatibility")
				confirmed := true
				saved.Companies[0].SLOConfirmed, saved.Companies[0].Groups[0].SLOConfirmed = &confirmed, &confirmed
				saved, err = SavePerformanceHealthConfig(saved)
				require.NoError(t, err)
				assert.True(t, *saved.Companies[0].SLOConfirmed)
				assert.True(t, *saved.Companies[0].Groups[0].SLOConfirmed)
				read, err := GetPerformanceHealthConfig()
				require.NoError(t, err)
				assert.Equal(t, saved, read)
			})

			t.Run("intentional removals stay removed after presets are applied", func(t *testing.T) {
				setPerformanceHealthConfigFixture(t, db, GetPerformanceHealthPresets())
				current, err := GetPerformanceHealthConfig()
				require.NoError(t, err)
				var removed string
				for i := range current.Companies {
					if current.Companies[i].ID == "openai" {
						group := &current.Companies[i].Groups[0]
						removed = group.Models[0]
						group.Models = group.Models[1:]
					}
				}
				require.NotEmpty(t, removed)
				saved, err := SavePerformanceHealthConfig(current)
				require.NoError(t, err)
				assert.Contains(t, saved.ExcludedModels, removed)
				preview, err := PreviewPerformanceHealthPresets()
				require.NoError(t, err)
				assert.NotContains(t, preview.AddedModels, removed)
				assert.Contains(t, preview.SkippedModels, removed)
			})

			t.Run("failed commit preserves database and memory", func(t *testing.T) {
				setPerformanceHealthConfigFixture(t, db, GetPerformanceHealthPresets())
				current, err := GetPerformanceHealthConfig()
				require.NoError(t, err)
				common.OptionMapRWMutex.RLock()
				cached := common.OptionMap[PerformanceHealthConfigOption]
				common.OptionMapRWMutex.RUnlock()
				require.NoError(t, db.Callback().Update().Before("gorm:update").Register("performance_health_reject", func(tx *gorm.DB) {
					tx.AddError(errors.New("write rejected"))
				}))
				current.Companies[0].Note = "must not commit"
				_, err = SavePerformanceHealthConfig(current)
				assert.ErrorContains(t, err, "write rejected")
				require.NoError(t, db.Callback().Update().Remove("performance_health_reject"))
				read, err := GetPerformanceHealthConfig()
				require.NoError(t, err)
				assert.NotEqual(t, "must not commit", read.Companies[0].Note)
				common.OptionMapRWMutex.RLock()
				assert.Equal(t, cached, common.OptionMap[PerformanceHealthConfigOption])
				common.OptionMapRWMutex.RUnlock()
			})

			t.Run("only one writer based on a version succeeds", func(t *testing.T) {
				setPerformanceHealthConfigFixture(t, db, GetPerformanceHealthPresets())
				current, err := GetPerformanceHealthConfig()
				require.NoError(t, err)
				results := make(chan error, 2)
				var writes sync.WaitGroup
				for range 2 {
					writes.Go(func() {
						_, err := SavePerformanceHealthConfig(current)
						results <- err
					})
				}
				writes.Wait()
				close(results)
				var successful, conflicted int
				for err := range results {
					if err == nil {
						successful++
					} else if errors.Is(err, ErrPerformanceHealthConflict) {
						conflicted++
					} else {
						t.Error(err)
					}
				}
				assert.Equal(t, 1, successful)
				assert.Equal(t, 1, conflicted)
			})

			t.Run("real read-only database rejects a save without changing memory", func(t *testing.T) {
				setPerformanceHealthConfigFixture(t, db, GetPerformanceHealthPresets())
				current, err := GetPerformanceHealthConfig()
				require.NoError(t, err)
				cachePerformanceHealthConfig(current)
				common.OptionMapRWMutex.RLock()
				cached := common.OptionMap[PerformanceHealthConfigOption]
				common.OptionMapRWMutex.RUnlock()
				attempt, err := clonePerformanceHealthConfig(current)
				require.NoError(t, err)
				attempt.Companies[0].Note = "must not commit"
				if dialect == "sqlite" {
					require.NoError(t, db.Exec("PRAGMA query_only = ON").Error)
					_, err = SavePerformanceHealthConfig(attempt)
					require.Error(t, err)
					require.NoError(t, db.Exec("PRAGMA query_only = OFF").Error)
				} else {
					readOnly := db.Begin(&sql.TxOptions{ReadOnly: true})
					require.NoError(t, readOnly.Error)
					DB = readOnly
					_, err = SavePerformanceHealthConfig(attempt)
					assert.Error(t, err)
					require.NoError(t, readOnly.Rollback().Error)
					DB = db
				}
				read, err := GetPerformanceHealthConfig()
				require.NoError(t, err)
				assert.Equal(t, current, read)
				common.OptionMapRWMutex.RLock()
				assert.Equal(t, cached, common.OptionMap[PerformanceHealthConfigOption])
				common.OptionMapRWMutex.RUnlock()
			})

			t.Run("deleted companies and generated groups are not recreated", func(t *testing.T) {
				config := GetPerformanceHealthPresets()
				config.Companies[0].Groups = nil
				config.Companies[1].Groups[0].ID = "openai-default"
				config.Companies[0].Groups = []PerformanceModelGroup{{ID: "openai-default-1", PresetKey: "moonshot", Name: "Generated", Models: []string{"kimi-k3"}, Enabled: true}}
				setPerformanceHealthConfigFixture(t, db, config)
				config.Companies[0].Groups = nil
				config.Companies = config.Companies[:2]
				saved, err := SavePerformanceHealthConfig(config)
				require.NoError(t, err)
				assert.Contains(t, saved.ExcludedCompanies, "anthropic")
				assert.Contains(t, saved.ExcludedGroups, "moonshot:default")
				preview, err := PreviewPerformanceHealthPresets()
				require.NoError(t, err)
				assert.Empty(t, preview.AddedCompanies)
				assert.Empty(t, preview.AddedGroups)
			})

			t.Run("colliding default group IDs remain idempotent", func(t *testing.T) {
				config, err := clonePerformanceHealthConfig(legacy)
				require.NoError(t, err)
				config.Companies[1].Groups[0].ID = "openai-default"
				setPerformanceHealthConfigFixture(t, db, config)
				preview, err := PreviewPerformanceHealthPresets()
				require.NoError(t, err)
				_, err = ApplyPerformanceHealthPresets(preview.ExpectedConfigVersion, preview.PreviewToken)
				require.NoError(t, err)
				preview, err = PreviewPerformanceHealthPresets()
				require.NoError(t, err)
				assert.Empty(t, preview.AddedGroups)
				assert.Empty(t, preview.AddedModels)
			})

			t.Run("configured observed and metadata models are proposed without claiming unknown aliases", func(t *testing.T) {
				type candidateChannel struct{ Models string }
				type candidateModel struct {
					Id        int `gorm:"primaryKey"`
					ModelName string
					VendorID  int
					NameRule  int
					DeletedAt gorm.DeletedAt
				}
				type candidateVendor struct {
					Id        int `gorm:"primaryKey"`
					Name      string
					DeletedAt gorm.DeletedAt
				}
				type observedModel struct{ ModelName string }
				channelTable := db.NamingStrategy.TableName("Channel")
				modelTable := db.NamingStrategy.TableName("Model")
				vendorTable := db.NamingStrategy.TableName("Vendor")
				perfTable := (PerfMetric{}).TableName()
				require.NoError(t, db.Table(channelTable).AutoMigrate(&candidateChannel{}))
				require.NoError(t, db.Table(modelTable).AutoMigrate(&candidateModel{}))
				require.NoError(t, db.Table(vendorTable).AutoMigrate(&candidateVendor{}))
				require.NoError(t, db.Table(perfTable).AutoMigrate(&observedModel{}))
				t.Cleanup(func() {
					require.NoError(t, db.Migrator().DropTable(channelTable, modelTable, vendorTable, perfTable))
				})
				setPerformanceHealthConfigFixture(t, db, legacy)
				require.NoError(t, db.Table(channelTable).Create(&candidateChannel{Models: "gpt-discovered,kimi-discovered,claude-discovered,codex-auto-review,private-claude-model,gpt-manual-vendor"}).Error)
				require.NoError(t, db.Table(vendorTable).Create(&[]candidateVendor{{Id: 1, Name: "Anthropic"}, {Id: 2, Name: "Internal"}}).Error)
				require.NoError(t, db.Table(modelTable).Create(&[]candidateModel{
					{Id: 1, ModelName: "private-claude-", NameRule: NameRulePrefix, VendorID: 1},
					{Id: 2, ModelName: "private-routed-id", NameRule: NameRuleExact, VendorID: 1},
					{Id: 3, ModelName: "gpt-manual-vendor", NameRule: NameRuleExact, VendorID: 2},
				}).Error)
				require.NoError(t, db.Table(perfTable).Create(&observedModel{ModelName: "gpt-from-history"}).Error)
				preview, err := PreviewPerformanceHealthPresets()
				require.NoError(t, err)
				for _, name := range []string{"gpt-discovered", "kimi-discovered", "claude-discovered", "private-claude-model", "private-routed-id", "gpt-from-history"} {
					assert.Contains(t, preview.AddedModels, name)
				}
				assert.Contains(t, preview.SkippedModels, "codex-auto-review")
				assert.Contains(t, preview.SkippedModels, "gpt-manual-vendor", "saved metadata overrides a family guess")
				require.NoError(t, db.Table(channelTable).Where("1 = 1").Update("models", "gpt-added-after-preview").Error)
				_, err = ApplyPerformanceHealthPresets(preview.ExpectedConfigVersion, preview.PreviewToken)
				assert.ErrorIs(t, err, ErrPerformanceHealthConflict, "candidate inventory changes require another preview")
			})
		})
	}
}

func TestValidatePerformanceHealthConfigNormalizesModelsAndDefaultsStatus(t *testing.T) {
	config := PerformanceHealthConfig{Companies: []PerformanceCompany{{
		ID:     "openai",
		Name:   "OpenAI",
		Groups: []PerformanceModelGroup{{ID: "general", Name: "General", Models: []string{" gpt-4.1 ", "gpt-4.1", ""}}},
	}}}

	require.NoError(t, validatePerformanceHealthConfig(config))
	require.Equal(t, "active", config.Companies[0].Status)
	require.Equal(t, []string{"gpt-4.1"}, config.Companies[0].Groups[0].Models)
}
