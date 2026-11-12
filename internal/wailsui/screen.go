package wailsui

import "github.com/wailsapp/wails/v3/pkg/application"

// 多显示器定位的辅助函数。
//
// 为什么用 Screen.Name 而不是 Screen.ID：Windows 后端的 Screen.ID 是 HMONITOR
// 的字符串形式（screen_windows.go: hMonitorToScreenID），句柄在重启/重新登录后
// 可能变化；而 Name 是 MONITORINFOEX.szDevice（"\\.\DISPLAY1"），跨会话稳定。
// 副屏坐标只有配上稳定的屏幕标识，下次启动才能回到同一块屏。

// workRectOf 把 Wails 屏幕的工作区转成 uiRect；无效时返回 false。
func workRectOf(sc *application.Screen) (uiRect, bool) {
	if sc == nil || sc.WorkArea.Width <= 0 || sc.WorkArea.Height <= 0 {
		return uiRect{}, false
	}
	wa := sc.WorkArea
	return uiRect{X: wa.X, Y: wa.Y, W: wa.Width, H: wa.Height}, true
}

// screensPrimary 返回主屏；没有标记主屏时退回第一块有效屏幕。
func screensPrimary(screens []*application.Screen) *application.Screen {
	for _, sc := range screens {
		if sc != nil && sc.IsPrimary {
			return sc
		}
	}
	for _, sc := range screens {
		if sc != nil {
			return sc
		}
	}
	return nil
}

// screenByName 按 Screen.Name 查找显示器（空名/未找到返回 nil）。
func screenByName(screens []*application.Screen, name string) *application.Screen {
	if name == "" {
		return nil
	}
	for _, sc := range screens {
		if sc != nil && sc.Name == name {
			return sc
		}
	}
	return nil
}

// screenNearestPoint 返回包含给定点（或离它最近）的显示器。
// 用于旧状态没有屏幕名、但记了绝对坐标时反查屏幕。
func screenNearestPoint(screens []*application.Screen, x, y int) *application.Screen {
	var best *application.Screen
	bestDist := -1
	for _, sc := range screens {
		if sc == nil {
			continue
		}
		r := sc.Bounds
		dist := 0
		if r.Width > 0 && r.Height > 0 {
			switch {
			case x < r.X:
				dist += r.X - x
			case x >= r.X+r.Width:
				dist += x - (r.X + r.Width - 1)
			}
			switch {
			case y < r.Y:
				dist += r.Y - y
			case y >= r.Y+r.Height:
				dist += y - (r.Y + r.Height - 1)
			}
		}
		if bestDist < 0 || dist < bestDist {
			best, bestDist = sc, dist
		}
	}
	return best
}
