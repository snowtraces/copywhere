package panel

import _ "embed"

//go:embed icon.ico
var iconICO []byte

// AppIcon 返回应用图标（ICO 字节）：Wails 托盘与窗口复用同一份资源，
// 避免仓库里出现多份相同的二进制图标。
func AppIcon() []byte { return iconICO }
