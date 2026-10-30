package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/glebarez/sqlite"
	"github.com/shirou/gopsutil/cpu"
	gnet "github.com/shirou/gopsutil/net"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestSystemResourceCounterRates(t *testing.T) {
	base := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	initialCPU := []cpu.TimesStat{{User: 10, Idle: 90}}
	initialNetwork := map[string]gnet.IOCountersStat{"eth0": {BytesRecv: 1000, BytesSent: 100}}
	var sampler systemResourceSampler
	first := sampler.advance(base, initialCPU, initialNetwork)
	assert.Nil(t, first.CPUPercent)
	assert.Nil(t, first.NetworkReceive)
	assert.Nil(t, first.NetworkSend)
	point := sampler.advance(base.Add(5*time.Second), []cpu.TimesStat{{User: 15, Idle: 95}}, map[string]gnet.IOCountersStat{"eth0": {BytesRecv: 6120, BytesSent: 2660}})
	require.NotNil(t, point.CPUPercent)
	require.NotNil(t, point.NetworkReceive)
	require.NotNil(t, point.NetworkSend)
	assert.Equal(t, 50.0, *point.CPUPercent)
	assert.Equal(t, 1024.0, *point.NetworkReceive)
	assert.Equal(t, 512.0, *point.NetworkSend)

	for _, tc := range []struct {
		name    string
		cpu     []cpu.TimesStat
		network map[string]gnet.IOCountersStat
		elapsed time.Duration
	}{
		{"counter reset", []cpu.TimesStat{{User: 1, Idle: 1}}, map[string]gnet.IOCountersStat{"eth0": {BytesRecv: 1, BytesSent: 1}}, 5 * time.Second},
		{"failed read", nil, nil, 5 * time.Second},
		{"stalled sampler", []cpu.TimesStat{{User: 20, Idle: 100}}, initialNetwork, time.Minute},
		{"clock moved backwards", initialCPU, initialNetwork, -time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sampler systemResourceSampler
			sampler.advance(base, initialCPU, initialNetwork)
			point := sampler.advance(base.Add(tc.elapsed), tc.cpu, tc.network)
			assert.Nil(t, point.CPUPercent)
			assert.Nil(t, point.NetworkReceive)
			assert.Nil(t, point.NetworkSend)
		})
	}

	// A replacement interface cannot inherit the old interface's byte count.
	point = sampler.advance(base.Add(10*time.Second), initialCPU, map[string]gnet.IOCountersStat{"eth1": {BytesRecv: 999999, BytesSent: 999999}})
	assert.Nil(t, point.NetworkReceive)
	assert.Nil(t, point.NetworkSend)
	point = sampler.advance(base.Add(15*time.Second), []cpu.TimesStat{{User: 10, Idle: 100}}, map[string]gnet.IOCountersStat{"eth1": {BytesRecv: 999999, BytesSent: 999999}})
	require.NotNil(t, point.CPUPercent)
	require.NotNil(t, point.NetworkReceive)
	assert.Zero(t, *point.CPUPercent, "actual idle CPU is zero, unlike a missing sample")
	assert.Zero(t, *point.NetworkReceive, "actual idle network is zero")
	sampler.advance(base.Add(20*time.Second), nil, nil)
	point = sampler.advance(base.Add(25*time.Second), initialCPU, initialNetwork)
	assert.Nil(t, point.CPUPercent, "a failed read clears the baseline")
	assert.Nil(t, point.NetworkReceive)
}

func TestSystemResourceDatabaseMatrix(t *testing.T) {
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			var driver gorm.Dialector
			switch dialect {
			case "sqlite":
				driver = sqlite.Open(filepath.Join(t.TempDir(), "resources.db"))
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
			db, err := gorm.Open(driver, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			oldDB := model.DB
			model.DB = db
			t.Cleanup(func() {
				model.DB = oldDB
				require.NoError(t, db.Migrator().DropTable(&model.SystemResourceSample{}))
				require.NoError(t, sqlDB.Close())
			})
			var version string
			versionQuery := "SELECT version()"
			if dialect == "sqlite" {
				versionQuery = "SELECT sqlite_version()"
			}
			require.NoError(t, db.Raw(versionQuery).Scan(&version).Error)
			t.Logf("%s version: %s", dialect, version)
			for range 2 {
				require.NoError(t, db.AutoMigrate(&model.SystemResourceSample{}))
			}
			ctx := context.Background()
			base := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC).Unix()
			node := "网关-東京-α"
			empty, err := model.ListSystemResourceSamples(ctx, node, base-43200, base)
			require.NoError(t, err)
			assert.Empty(t, empty, "fresh installs cannot invent 12 hours of history")
			percent, zero := 37.5, 0.0
			used, total := uint64(6<<30), uint64(16<<30)
			first := model.SystemResourcePoint{Timestamp: base, CPUPercent: &percent, MemoryUsed: &used, MemoryTotal: &total, NetworkReceive: &zero}
			require.NoError(t, model.SaveSystemResourceSample(ctx, node, first))
			require.NoError(t, model.SaveSystemResourceSample(ctx, node, first))
			missing := model.SystemResourcePoint{Timestamp: base + 180}
			require.NoError(t, model.SaveSystemResourceSample(ctx, node, missing))
			require.NoError(t, model.SaveSystemResourceSample(ctx, "another-node", first))
			expired := first
			expired.Timestamp = base - int64(SystemResourceRetention/time.Second) - 60
			require.NoError(t, model.SaveSystemResourceSample(ctx, node, expired))
			// Repeated migration with existing data must preserve nulls, zeroes,
			// large byte quantities, node isolation and composite uniqueness.
			for range 2 {
				require.NoError(t, db.AutoMigrate(&model.SystemResourceSample{}))
			}
			points, err := model.ListSystemResourceSamples(ctx, node, base-1, base+180)
			require.NoError(t, err)
			assert.Equal(t, []model.SystemResourcePoint{first, missing}, points)
			filtered, err := model.ListSystemResourceSamples(ctx, node, base+1, base+180)
			require.NoError(t, err)
			assert.Equal(t, []model.SystemResourcePoint{missing}, filtered)
			require.NoError(t, model.DeleteExpiredSystemResourceSamples(ctx, base-int64(SystemResourceRetention/time.Second)))
			all, err := model.ListSystemResourceSamples(ctx, node, expired.Timestamp, base+180)
			require.NoError(t, err)
			assert.Equal(t, points, all)
			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			_, err = model.ListSystemResourceSamples(cancelled, node, base-1, base+180)
			require.ErrorIs(t, err, context.Canceled)
		})
	}
}
