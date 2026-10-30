package common

import (
	"bytes"
	"encoding/base64"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"strings"

	_ "golang.org/x/image/webp"
)

// ValidateLogo validates inline uploads while preserving existing logo URLs.
func ValidateLogo(value string) error {
	value = strings.TrimSpace(value)
	if len(value) < 5 || !strings.EqualFold(value[:5], "data:") {
		return nil
	}
	const maxBytes = 512 * 1024
	header, encoded, found := strings.Cut(value, ",")
	format := ""
	switch strings.ToLower(header) {
	case "data:image/png;base64":
		format = "png"
	case "data:image/jpeg;base64":
		format = "jpeg"
	case "data:image/webp;base64":
		format = "webp"
	default:
		return NewMessage("Choose a PNG, JPG, or WebP image")
	}
	if len(encoded) > base64.StdEncoding.EncodedLen(maxBytes) {
		return NewMessage("Logo image must be 512 KB or smaller")
	}
	data, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if !found || err != nil || len(data) == 0 {
		return NewMessage("Unable to read this image. Choose a valid PNG, JPG, or WebP file.")
	}
	if len(data) > maxBytes {
		return NewMessage("Logo image must be 512 KB or smaller")
	}
	config, actualFormat, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || actualFormat != format {
		return NewMessage("Unable to read this image. Choose a valid PNG, JPG, or WebP file.")
	}
	if config.Width < 1 || config.Height < 1 || config.Width > 2048 || config.Height > 2048 {
		return NewMessage("Logo dimensions must not exceed 2048 × 2048 pixels")
	}
	if _, _, err = image.Decode(bytes.NewReader(data)); err != nil {
		return NewMessage("Unable to read this image. Choose a valid PNG, JPG, or WebP file.")
	}
	return nil
}
