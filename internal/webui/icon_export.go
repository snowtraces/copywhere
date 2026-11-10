package webui

// AppIcon 返回应用图标（ICO 字节）。
// 供其它前端（例如 Wails 桌面窗口）复用同一份图标资源，避免仓库里出现多份
// 相同的二进制图标。
func AppIcon() []byte { return iconICO }

// FaviconPNG 返回应用 PNG 图标（192x192），供窗口/关于页复用。
func FaviconPNG() []byte { return faviconPNG }
