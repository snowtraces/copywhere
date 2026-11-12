//go:build !windows

package wailsui

import (
	"errors"
	"time"
)

// 非 Windows 平台没有"系统最小窗口宽度"与分层窗口的差异，样式增强全部空实现。
func setFramelessDockStyle(uintptr) {}
func restoreCaptionStyle(uintptr)   {}
func setDockChrome(uintptr, bool)   {}

// startWindowDrag：没有原生拖动循环，明确报错让前端把这手势当普通点击处理。
func startWindowDrag(uintptr) error {
	return errors.New("仅 Windows 支持拖动吸边图标")
}

// leftMouseDown：非 Windows 平台没有这个拖动检测，按"已松手"处理。
func leftMouseDown() bool { return false }

// trayMenuDelay：没有系统双击间隔可查，用一个居中的固定值。
func trayMenuDelay() time.Duration { return 300 * time.Millisecond }
