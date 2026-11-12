package wailsui

import "strings"

// 界面模式：标准宽窗口（顶栏 + 设备坞 + 工作台）与简约竖窄面板（QQ 式）。
const (
	UIModeStandard = "standard"
	UIModeMini     = "mini"
)

// 简约界面的窗口尺寸（DIP，与 WebView 的 CSS 像素 1:1）。
const (
	miniWidth  = 320 // 展开面板宽度
	miniHeight = 520 // 展开面板高度
	dockSize   = 64  // 吸边收起后的图标窗口边长（方形，只显示应用图标）

	// miniEdgeGap 是展开面板与屏幕边缘留的缝。它必须大于 edgeSnapTol，
	// 否则"从图标展开"后的面板本身就算贴着边缘，会被下一次位置回调立刻收起。
	miniEdgeGap = 16
	// edgeSnapTol 是拖到距工作区左/右边缘多近判定为"吸边"。
	edgeSnapTol = 10
)

// UiState 是界面模式快照：前端据此切换外壳，窗口控制器据此落盘并在下次启动恢复。
type UiState struct {
	Mode        string `json:"mode"`
	Collapsed   bool   `json:"collapsed"`
	Edge        string `json:"edge"` // "left" | "right"
	AlwaysOnTop bool   `json:"alwaysOnTop"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
}

// normalizeUIMode 把任意输入收敛到两种模式之一（未知值按标准界面处理）。
func normalizeUIMode(mode string) string {
	if strings.EqualFold(strings.TrimSpace(mode), UIModeMini) {
		return UIModeMini
	}
	return UIModeStandard
}

// normalizeEdge 把任意输入收敛到 left/right（未知值按右侧处理，右侧更常见）。
func normalizeEdge(edge string) string {
	if strings.EqualFold(strings.TrimSpace(edge), "left") {
		return "left"
	}
	return "right"
}

// uiRect 是 DIP 矩形。刻意不复用 application.Rect：
// 几何计算不需要 Wails 运行时，可以脱离窗口单测。
type uiRect struct {
	X, Y, W, H int
}

func (r uiRect) right() int   { return r.X + r.W }
func (r uiRect) bottom() int  { return r.Y + r.H }
func (r uiRect) centerY() int { return r.Y + r.H/2 }
func (r uiRect) centerX() int { return r.X + r.W/2 }

// clampInt 把 v 夹在 [lo, hi]；hi < lo（工作区比窗口还小）时退化为 lo。
func clampInt(v, lo, hi int) int {
	if hi < lo {
		return lo
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// dockedRect 计算吸边收起后的图标窗口矩形：贴住工作区左/右边缘，
// 纵向以 anchorY 为中心并夹在工作区内。
func dockedRect(work uiRect, size, anchorY int, edge string) uiRect {
	y := clampInt(anchorY-size/2, work.Y, work.bottom()-size)
	x := work.X
	if normalizeEdge(edge) == "right" {
		x = work.right() - size
	}
	return uiRect{X: x, Y: y, W: size, H: size}
}

// panelRect 按给定的中心点 (cx, cy) 放置简约面板，并把整个矩形夹回工作区。
// 面板的横向位置也要记住：用户可以把它拖到屏幕中间，展开/重启时按原位置恢复，
// 不再强制贴回左/右边缘。
func panelRect(work uiRect, w, h, cx, cy int) uiRect {
	maxX := work.right() - w
	if maxX < work.X {
		maxX = work.X
	}
	maxY := work.bottom() - h
	if maxY < work.Y {
		maxY = work.Y
	}
	return uiRect{
		X: clampInt(cx-w/2, work.X, maxX),
		Y: clampInt(cy-h/2, work.Y, maxY),
		W: w,
		H: h,
	}
}

// floatingPanelRect 计算"还没有记住横向位置"时的简约面板矩形：贴住 edge 留 gap，
// 纵向以 anchorY 为中心。anchorY <= 0 表示没有记录过位置，纵向居中。
func floatingPanelRect(work uiRect, w, h, anchorY, gap int, edge string) uiRect {
	if anchorY <= 0 {
		anchorY = work.centerY()
	}
	x := work.X + gap + w/2
	if normalizeEdge(edge) == "right" {
		x = work.right() - gap - w/2
	}
	return panelRect(work, w, h, x, anchorY)
}

// centeredRect 把窗口在工作区内居中（回到标准界面时用：比"居中到主屏"更稳，
// 显示器布局变化后也不会跑到屏幕外）。
func centeredRect(work uiRect, w, h int) uiRect {
	maxX := work.right() - w
	if maxX < work.X {
		maxX = work.X
	}
	maxY := work.bottom() - h
	if maxY < work.Y {
		maxY = work.Y
	}
	return uiRect{
		X: clampInt(work.X+(work.W-w)/2, work.X, maxX),
		Y: clampInt(work.Y+(work.H-h)/2, work.Y, maxY),
		W: w,
		H: h,
	}
}

// edgeHit 判断窗口是否已经贴上工作区的左/右边缘（tol 为容差）。
// 窗口被拖出屏幕外时差值为负，同样算贴边。同时贴住两边时取更近的一侧。
func edgeHit(work, win uiRect, tol int) string {
	leftD := win.X - work.X
	rightD := work.right() - win.right()
	switch {
	case leftD <= tol && leftD <= rightD:
		return "left"
	case rightD <= tol:
		return "right"
	}
	return ""
}

// nearestEdge 返回窗口中心更靠近的一侧边缘（托盘/按钮主动收起时用）。
func nearestEdge(work, win uiRect) string {
	if win.centerX()-work.X <= work.right()-win.centerX() {
		return "left"
	}
	return "right"
}
