package wailsui

import (
	"encoding/base64"
	"path/filepath"
	"strings"
)

// imageExts 是窗口图片预览的白名单。
var imageExts = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".gif": "image/gif", ".bmp": "image/bmp", ".webp": "image/webp",
}

// imageContentType 返回扩展名对应的 Content-Type；非图片返回 false。
func imageContentType(path string) (string, bool) {
	ct, ok := imageExts[strings.ToLower(filepath.Ext(path))]
	return ct, ok
}

// base64Encode 是 base64.StdEncoding 的短别名，避免在 service.go 顶部多导一个包。
func base64Encode(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
