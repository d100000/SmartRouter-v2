package model

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func logoPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	var data bytes.Buffer
	encoder := png.Encoder{CompressionLevel: png.NoCompression}
	require.NoError(t, encoder.Encode(&data, image.NewNRGBA(image.Rect(0, 0, width, height))))
	return data.Bytes()
}

func TestLogoImageValidation(t *testing.T) {
	pngData := logoPNG(t, 2, 2)
	pngURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngData)
	var jpegData bytes.Buffer
	require.NoError(t, jpeg.Encode(&jpegData, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil))
	for _, tc := range []struct {
		name, value, errorKey string
	}{
		{name: "default", value: ""},
		{name: "existing remote SVG", value: "https://example.com/logo.svg"},
		{name: "existing relative logo", value: "/custom-logo.png"},
		{name: "PNG", value: pngURL},
		{name: "WebP", value: "data:image/webp;base64,UklGRkwAAABXRUJQVlA4WAoAAAAQAAAAAAAAAAAAQUxQSAIAAAAAPVZQOCAkAAAAcAEAnQEqAQABAAIANCWgAnQBQAAA/u3tZ/4IL+IL+IKzpJAA"},
		{name: "JPEG", value: "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(jpegData.Bytes())},
		{name: "512 KB boundary", value: "data:image/png;base64," + base64.StdEncoding.EncodeToString(append(bytes.Clone(pngData), make([]byte, 512*1024-len(pngData))...))},
		{name: "unsupported type", value: "data:image/svg+xml;base64,PHN2Zz48L3N2Zz4=", errorKey: "Choose a PNG, JPG, or WebP image"},
		{name: "case insensitive data scheme", value: " DATA:image/svg+xml;base64,PHN2Zz48L3N2Zz4=", errorKey: "Choose a PNG, JPG, or WebP image"},
		{name: "empty", value: "data:image/png;base64,", errorKey: "Unable to read this image. Choose a valid PNG, JPG, or WebP file."},
		{name: "invalid base64", value: "data:image/png;base64,%%%", errorKey: "Unable to read this image. Choose a valid PNG, JPG, or WebP file."},
		{name: "MIME mismatch", value: "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(pngData), errorKey: "Unable to read this image. Choose a valid PNG, JPG, or WebP file."},
		{name: "truncated image", value: "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngData[:len(pngData)-12]), errorKey: "Unable to read this image. Choose a valid PNG, JPG, or WebP file."},
		{name: "too many bytes", value: "data:image/png;base64," + base64.StdEncoding.EncodeToString(append(bytes.Clone(pngData), make([]byte, 512*1024+1-len(pngData))...)), errorKey: "Logo image must be 512 KB or smaller"},
		{name: "oversized encoding", value: "data:image/png;base64," + strings.Repeat("A", 700000), errorKey: "Logo image must be 512 KB or smaller"},
		{name: "excessive dimensions", value: "data:image/png;base64," + base64.StdEncoding.EncodeToString(logoPNG(t, 2049, 1)), errorKey: "Logo dimensions must not exceed 2048 × 2048 pixels"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := common.ValidateLogo(tc.value)
			if tc.errorKey == "" {
				require.NoError(t, err)
				return
			}
			var message *common.Message
			require.ErrorAs(t, err, &message)
			assert.Equal(t, tc.errorKey, message.Key)
		})
	}
}

func TestLogoOptionDatabaseMatrix(t *testing.T) {
	logo := "data:image/png;base64," + base64.StdEncoding.EncodeToString(logoPNG(t, 256, 256))
	require.Greater(t, len(logo), 65535, "exercise images exceeding a short TEXT column")
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			var driver gorm.Dialector
			switch dialect {
			case "sqlite":
				driver = sqlite.Open(filepath.Join(t.TempDir(), "logo.db"))
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
			oldDB, oldLogo, oldOptions := DB, common.Logo, common.OptionMap
			DB, common.Logo, common.OptionMap = db, "", make(map[string]string)
			t.Cleanup(func() {
				DB, common.Logo, common.OptionMap = oldDB, oldLogo, oldOptions
				require.NoError(t, db.Migrator().DropTable(&Option{}))
				require.NoError(t, sqlDB.Close())
			})
			query := "SELECT version()"
			if dialect == "sqlite" {
				query = "SELECT sqlite_version()"
			}
			var version string
			require.NoError(t, db.Raw(query).Scan(&version).Error)
			t.Logf("%s version: %s", dialect, version)
			require.NoError(t, db.AutoMigrate(&Option{}))
			require.NoError(t, UpdateOption("Logo", "https://example.com/old.svg"))
			require.NoError(t, UpdateOption("Logo", logo))
			for range 2 {
				require.NoError(t, db.AutoMigrate(&Option{}))
			}
			assert.Equal(t, logo, requireOptionValue(t, db, "Logo"))
			assert.Equal(t, logo, common.Logo)
			assert.Equal(t, logo, common.OptionMap["Logo"])

			require.Error(t, UpdateOption("Logo", "data:image/png;base64,broken"))
			assert.Equal(t, logo, requireOptionValue(t, db, "Logo"))
			assert.Equal(t, logo, common.Logo)

			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			DB = db.WithContext(ctx)
			require.ErrorIs(t, UpdateOption("Logo", ""), context.Canceled)
			assert.Equal(t, logo, requireOptionValue(t, db, "Logo"))
			assert.Equal(t, logo, common.Logo, "failed writes must not publish an unsaved logo")
			assert.Equal(t, logo, common.OptionMap["Logo"])
			DB = db
			require.NoError(t, UpdateOption("Logo", ""))
			assert.Empty(t, requireOptionValue(t, db, "Logo"))
			assert.Empty(t, common.Logo)
		})
	}
}
