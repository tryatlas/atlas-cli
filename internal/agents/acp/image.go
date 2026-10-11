package acp

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

var imageTypes = map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp"}

// MaxImageBytes bounds an attached image.
const MaxImageBytes = 20 << 20

// ImageBlock reads an image file into a prompt image block.
func ImageBlock(path string) (ContentBlock, error) {
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, path[2:])
		}
	}
	mime, ok := imageTypes[strings.ToLower(filepath.Ext(path))]
	if !ok {
		return ContentBlock{}, errors.New(filepath.Base(path) + " is not a png, jpeg, gif or webp file")
	}
	b, err := os.ReadFile(path) //nolint:gosec // G304: a file the user chose
	if err != nil {
		return ContentBlock{}, err
	}
	if len(b) > MaxImageBytes {
		return ContentBlock{}, errors.New(filepath.Base(path) + " is larger than 20 MiB")
	}
	return ContentBlock{Type: "image", Data: base64.StdEncoding.EncodeToString(b), MimeType: mime}, nil
}
