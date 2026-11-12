package wailsui

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// ---------- 假窗口 ----------

// fakeWindow 模拟 Wails 窗口的几何行为（含 SetMinSize/SetMaxSize 的即时收缩，
// 真实实现在 Windows 上就是这样：设上限时窗口立刻变小）。
type fakeWindow struct {
	x, y, w, h int
	minW, minH int
	maxW, maxH int

	frameless bool
	resizable bool
	top       bool
	hidden    bool
	screen    *application.Screen
	native    uintptr
}

func (f *fakeWindow) Size() (int, int)     { return f.w, f.h }
func (f *fakeWindow) Position() (int, int) { return f.x, f.y }

func (f *fakeWindow) SetSize(w, h int) { f.w, f.h = w, h }

func (f *fakeWindow) SetPosition(x, y int) { f.x, f.y = x, y }

func (f *fakeWindow) SetMinSize(w, h int) {
	f.minW, f.minH = w, h
	if w > 0 && f.w < w {
		f.w = w
	}
	if h > 0 && f.h < h {
		f.h = h
	}
}

func (f *fakeWindow) SetMaxSize(w, h int) {
	f.maxW, f.maxH = w, h
	if w > 0 && f.w > w {
		f.w = w
	}
	if h > 0 && f.h > h {
		f.h = h
	}
}

func (f *fakeWindow) SetResizable(v bool)   { f.resizable = v }
func (f *fakeWindow) SetFrameless(v bool)   { f.frameless = v }
func (f *fakeWindow) SetAlwaysOnTop(v bool) { f.top = v }
func (f *fakeWindow) Show()                 { f.hidden = false }
func (f *fakeWindow) Focus()                {}
func (f *fakeWindow) Hide()                 { f.hidden = true }

// NativeWindow 默认返回 0：单测不碰真正的 Win32 窗口样式。
// 需要走 ApplyNativeStyle 的用例会显式把 native 置为非 0。
func (f *fakeWindow) NativeWindow() uintptr { return f.native }
func (f *fakeWindow) GetScreen() (*application.Screen, error) {
	return f.screen, nil
}

// testWorkArea 是 1920x1040 的单屏工作区（任务栏占 40）。
var testWorkArea = application.Rect{X: 0, Y: 0, Width: 1920, Height: 1040}

// testPrimaryScreen 是单屏环境的主显示器（Name 用 Windows 的 \\.\DISPLAY1 形式）。
var testPrimaryScreen = &application.Screen{
	ID: "1", Name: `\\.\DISPLAY1`, IsPrimary: true,
	WorkArea: testWorkArea, Bounds: testWorkArea,
}

func testScreens() []*application.Screen {
	return []*application.Screen{testPrimaryScreen}
}

// testSecondScreen 是主屏右侧的副屏（1280x984 工作区，任务栏占 40）。
var testSecondScreen = &application.Screen{
	ID: "2", Name: `\\.\DISPLAY2`,
	WorkArea: application.Rect{X: 1920, Y: 0, Width: 1280, Height: 984},
	Bounds:   application.Rect{X: 1920, Y: 0, Width: 1280, Height: 1024},
}

func testScreensTwo() []*application.Screen {
	return []*application.Screen{testPrimaryScreen, testSecondScreen}
}

// fakeStyle 记录 setMini 的调用（单测不碰真正的 Win32 样式）。
type fakeStyle struct{ mini bool }

func (f *fakeStyle) setMini(on bool) { f.mini = on }

func newFakeWindow() *fakeWindow {
	return &fakeWindow{
		x: 400, y: 140, w: defaultWidth, h: defaultHeight,
		screen: testPrimaryScreen,
	}
}

func newTestController(t *testing.T, st windowState) (*windowController, *fakeWindow) {
	t.Helper()
	fw := newFakeWindow()
	if st.Width < minWidth {
		st.Width = defaultWidth
	}
	if st.Height < minHeight {
		st.Height = defaultHeight
	}
	ctl := newWindowController(fw, &fakeStyle{}, filepath.Join(t.TempDir(), "wails-window.json"), st, testScreens)
	// 单测不等真实时间窗。
	ctl.settleDelay = 2 * time.Millisecond
	ctl.guardWindow = time.Millisecond
	ctl.saveDelay = 2 * time.Millisecond
	// 不让测试依赖真实鼠标按键状态。
	ctl.mouseDown = func() bool { return false }
	// 去抖落盘是异步的：测试结束时先停掉定时器并落盘，避免在 TempDir 清理后又写文件。
	t.Cleanup(ctl.Stop)
	return ctl, fw
}

// waitFor 轮询等待条件成立（去抖/定时器都是异步的）。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("等待超时：%s", what)
}

func readStateFile(t *testing.T, ctl *windowController) windowState {
	t.Helper()
	data, err := os.ReadFile(ctl.path)
	if err != nil {
		t.Fatalf("读取窗口状态文件: %v", err)
	}
	var st windowState
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("解析窗口状态文件: %v", err)
	}
	return st
}

// ---------- 几何 ----------

func TestDockedRect(t *testing.T) {
	work := uiRect{X: 0, Y: 0, W: 1920, H: 1040}
	cases := []struct {
		name    string
		size    int
		anchorY int
		edge    string
		want    uiRect
	}{
		{"贴右边缘", 64, 500, "right", uiRect{X: 1856, Y: 468, W: 64, H: 64}},
		{"贴左边缘", 64, 500, "left", uiRect{X: 0, Y: 468, W: 64, H: 64}},
		{"纵向顶到工作区上沿", 64, 10, "right", uiRect{X: 1856, Y: 0, W: 64, H: 64}},
		{"纵向顶到工作区下沿", 64, 1039, "right", uiRect{X: 1856, Y: 976, W: 64, H: 64}},
		{"未知边缘按右侧", 64, 500, "up", uiRect{X: 1856, Y: 468, W: 64, H: 64}},
	}
	for _, c := range cases {
		if got := dockedRect(work, c.size, c.anchorY, c.edge); got != c.want {
			t.Errorf("%s: dockedRect = %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestFloatingPanelRect(t *testing.T) {
	work := uiRect{X: 0, Y: 0, W: 1920, H: 1040}
	if got := floatingPanelRect(work, 320, 520, 520, 16, "right"); got != (uiRect{X: 1584, Y: 260, W: 320, H: 520}) {
		t.Errorf("右侧面板 = %+v", got)
	}
	if got := floatingPanelRect(work, 320, 520, 520, 16, "left"); got != (uiRect{X: 16, Y: 260, W: 320, H: 520}) {
		t.Errorf("左侧面板 = %+v", got)
	}
	// anchorY = 0 表示没记录过位置：纵向居中。
	if got := floatingPanelRect(work, 320, 520, 0, 16, "left"); got.Y != 260 {
		t.Errorf("未记录锚点应纵向居中，实际 %+v", got)
	}
	// 纵向越界要夹回工作区。
	if got := floatingPanelRect(work, 320, 520, 5, 16, "left"); got.Y != 0 {
		t.Errorf("上越界应夹到 0，实际 %+v", got)
	}
	if got := floatingPanelRect(work, 320, 520, 1035, 16, "left"); got.Y != 520 {
		t.Errorf("下越界应夹到 520，实际 %+v", got)
	}
	// 工作区比窗口还小：贴住左上角，不能出现负坐标。
	small := uiRect{X: 0, Y: 0, W: 300, H: 400}
	if got := floatingPanelRect(small, 320, 520, 0, 16, "right"); got != (uiRect{X: 0, Y: 0, W: 320, H: 520}) {
		t.Errorf("小屏应退化到左上角，实际 %+v", got)
	}
}

func TestPanelRect(t *testing.T) {
	work := uiRect{X: 0, Y: 0, W: 1920, H: 1040}
	// 面板可以停在屏幕中间：按中心点恢复。
	if got := panelRect(work, 320, 520, 660, 560); got != (uiRect{X: 500, Y: 300, W: 320, H: 520}) {
		t.Errorf("屏幕中间 = %+v", got)
	}
	// 越界要夹回工作区，不能跑出屏幕。
	if got := panelRect(work, 320, 520, -100, -100); got != (uiRect{X: 0, Y: 0, W: 320, H: 520}) {
		t.Errorf("左上越界 = %+v", got)
	}
	if got := panelRect(work, 320, 520, 5000, 5000); got.X != 1600 || got.Y != 520 {
		t.Errorf("右下越界 = %+v", got)
	}
	// 工作区比面板还小：贴住左上角，不出现负坐标。
	small := uiRect{X: 0, Y: 0, W: 300, H: 400}
	if got := panelRect(small, 320, 520, 150, 200); got != (uiRect{X: 0, Y: 0, W: 320, H: 520}) {
		t.Errorf("小屏 = %+v", got)
	}
}

func TestCenteredRect(t *testing.T) {
	work := uiRect{X: 0, Y: 0, W: 1920, H: 1040}
	if got := centeredRect(work, 1120, 760); got != (uiRect{X: 400, Y: 140, W: 1120, H: 760}) {
		t.Errorf("centeredRect = %+v", got)
	}
}

func TestEdgeHitAndNearest(t *testing.T) {
	work := uiRect{X: 0, Y: 0, W: 1920, H: 1040}
	if got := edgeHit(work, uiRect{X: 0, Y: 200, W: 320, H: 520}, edgeSnapTol); got != "left" {
		t.Errorf("贴左边缘应命中 left，实际 %q", got)
	}
	if got := edgeHit(work, uiRect{X: 1920 - 320 - 4, Y: 200, W: 320, H: 520}, edgeSnapTol); got != "right" {
		t.Errorf("贴右边缘应命中 right，实际 %q", got)
	}
	if got := edgeHit(work, uiRect{X: -120, Y: 200, W: 320, H: 520}, edgeSnapTol); got != "left" {
		t.Errorf("拖出屏幕左侧也应命中 left，实际 %q", got)
	}
	if got := edgeHit(work, uiRect{X: 600, Y: 200, W: 320, H: 520}, edgeSnapTol); got != "" {
		t.Errorf("屏幕中间不该命中，实际 %q", got)
	}
	if got := nearestEdge(work, uiRect{X: 200, Y: 200, W: 320, H: 520}); got != "left" {
		t.Errorf("靠左应返回 left，实际 %q", got)
	}
	if got := nearestEdge(work, uiRect{X: 1400, Y: 200, W: 320, H: 520}); got != "right" {
		t.Errorf("靠右应返回 right，实际 %q", got)
	}
}

// ---------- 状态文件 ----------

func TestReadWindowStateDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wails-window.json")
	st := readWindowState(path)
	if st.Width != defaultWidth || st.Height != defaultHeight {
		t.Errorf("默认尺寸 = %dx%d", st.Width, st.Height)
	}
	if st.Mode != UIModeStandard || st.Edge != "right" || st.Pinned != nil {
		t.Errorf("默认偏好不符: %+v", st)
	}
	if st.AnchorY != 0 || st.PanelCX != 0 || st.DockAnchorY != 0 {
		t.Errorf("默认锚点应为 0，实际 panel=(%d,%d) dock=%d", st.PanelCX, st.AnchorY, st.DockAnchorY)
	}
	if st.Collapsed {
		t.Error("默认不应是收起状态")
	}
}

func TestWindowStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wails-window.json")
	// 旧版本只写尺寸：新字段必须能容忍缺省。
	if err := os.WriteFile(path, []byte(`{"width":1000,"height":700}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st := readWindowState(path)
	if st.Width != 1000 || st.Height != 700 || st.Mode != UIModeStandard {
		t.Fatalf("旧格式解析不符: %+v", st)
	}

	pinned := true
	windowStateWrite(path, windowState{
		Width: 1000, Height: 700, Mode: UIModeMini,
		Pinned: &pinned, Edge: "left", PanelCX: 660, AnchorY: 480, DockAnchorY: 700, Collapsed: true,
	})
	st = readWindowState(path)
	if st.Mode != UIModeMini || st.Edge != "left" || st.PanelCX != 660 || st.AnchorY != 480 || st.DockAnchorY != 700 || !st.Collapsed {
		t.Fatalf("简约界面状态未保留: %+v", st)
	}
	if st.Pinned == nil || !*st.Pinned {
		t.Fatalf("置顶偏好未保留: %+v", st.Pinned)
	}

	// 收起标记只对简约界面有效：标准界面里的残留值不能把窗口建成 64x64。
	windowStateWrite(path, windowState{Width: 1000, Height: 700, Mode: UIModeStandard, Collapsed: true})
	if st = readWindowState(path); st.Collapsed {
		t.Fatalf("标准界面不应保留收起状态: %+v", st)
	}

	// 越界尺寸回退默认，避免显示器变小后窗口跑到屏幕外。
	windowStateWrite(path, windowState{Width: 10, Height: 10})
	st = readWindowState(path)
	if st.Width != defaultWidth || st.Height != defaultHeight {
		t.Fatalf("越界尺寸应回退默认: %+v", st)
	}
}

// ---------- 控制器 ----------

func TestSwitchToMiniAndBack(t *testing.T) {
	ctl, fw := newTestController(t, windowState{})
	if err := ctl.SetMode(UIModeMini); err != nil {
		t.Fatalf("切到简约界面: %v", err)
	}
	if !fw.frameless {
		t.Error("简约界面应为无边框窗口")
	}
	if fw.resizable {
		t.Error("简约面板不应可缩放")
	}
	if fw.w != miniWidth || fw.h != miniHeight {
		t.Errorf("简约面板尺寸 = %dx%d", fw.w, fw.h)
	}
	if want := 1920 - miniWidth - miniEdgeGap; fw.x != want {
		t.Errorf("简约面板 x = %d, want %d（贴右边缘留缝）", fw.x, want)
	}
	if fw.top {
		t.Error("未点置顶时不应置顶")
	}

	st := ctl.State()
	if st.Mode != UIModeMini || st.Collapsed || st.Edge != "right" {
		t.Fatalf("状态快照不符: %+v", st)
	}

	// 切回标准界面：恢复原尺寸（工作区居中），且必须是可缩放带边框窗口。
	if err := ctl.SetMode(UIModeStandard); err != nil {
		t.Fatalf("切回标准界面: %v", err)
	}
	if fw.frameless || !fw.resizable {
		t.Error("标准界面应恢复边框与缩放")
	}
	if fw.w != defaultWidth || fw.h != defaultHeight {
		t.Errorf("标准窗口尺寸 = %dx%d", fw.w, fw.h)
	}
	if fw.x != 400 || fw.y != 140 {
		t.Errorf("标准窗口应居中，实际 (%d,%d)", fw.x, fw.y)
	}
	if st := ctl.State(); st.Mode != UIModeStandard || st.AlwaysOnTop {
		t.Fatalf("回到标准界面状态不符: %+v", st)
	}
}

func TestMiniPinAndCollapse(t *testing.T) {
	ctl, fw := newTestController(t, windowState{})
	if err := ctl.SetMode(UIModeMini); err != nil {
		t.Fatal(err)
	}
	if err := ctl.SetPinned(true); err != nil {
		t.Fatal(err)
	}
	if !fw.top {
		t.Error("置顶开关应生效")
	}

	if err := ctl.SetCollapsed(true); err != nil {
		t.Fatalf("收起: %v", err)
	}
	if fw.w != dockSize || fw.h != dockSize {
		t.Errorf("收起后尺寸 = %dx%d, want %d", fw.w, fw.h, dockSize)
	}
	if want := 1920 - dockSize; fw.x != want {
		t.Errorf("收起后应贴右边缘，x = %d, want %d", fw.x, want)
	}
	if !fw.top {
		t.Error("收起为图标时必须置顶，否则会被其它窗口盖住")
	}
	if st := ctl.State(); !st.Collapsed || !st.AlwaysOnTop {
		t.Fatalf("收起状态快照不符: %+v", st)
	}

	if err := ctl.SetCollapsed(false); err != nil {
		t.Fatalf("展开: %v", err)
	}
	if fw.w != miniWidth || fw.h != miniHeight {
		t.Errorf("展开后尺寸 = %dx%d", fw.w, fw.h)
	}
	if want := 1920 - miniWidth - miniEdgeGap; fw.x != want {
		t.Errorf("展开后应贴回右边缘，x = %d, want %d", fw.x, want)
	}
	if !fw.top {
		t.Error("置顶开关为开时，展开后应保持置顶")
	}

	// 关掉置顶后收起仍然临时置顶，展开后恢复不置顶。
	if err := ctl.SetPinned(false); err != nil {
		t.Fatal(err)
	}
	if fw.top {
		t.Error("关掉置顶后窗口不应再置顶")
	}
	if err := ctl.SetCollapsed(true); err != nil {
		t.Fatal(err)
	}
	if !fw.top {
		t.Error("未置顶时收起仍应临时置顶")
	}
	if err := ctl.SetCollapsed(false); err != nil {
		t.Fatal(err)
	}
	if fw.top {
		t.Error("展开后应恢复「未置顶」")
	}
}

func TestCollapseOnlyInMini(t *testing.T) {
	ctl, _ := newTestController(t, windowState{})
	if err := ctl.SetCollapsed(true); err == nil {
		t.Fatal("标准界面下收起应报错")
	}
}

func TestDragToEdgeCollapses(t *testing.T) {
	ctl, fw := newTestController(t, windowState{})
	if err := ctl.SetMode(UIModeMini); err != nil {
		t.Fatal(err)
	}
	// 模拟用户把面板拖到左边缘。
	fw.x = 3
	fw.y = 100
	ctl.onMoved()
	waitFor(t, "吸边收起", func() bool {
		st := ctl.State()
		return st.Collapsed && st.Edge == "left"
	})
	if fw.w != dockSize {
		t.Errorf("吸边后宽度 = %d, want %d", fw.w, dockSize)
	}
	if fw.x != 0 {
		t.Errorf("吸边后应贴住左边缘，x = %d", fw.x)
	}
}

func TestDragInMiddleKeepsAnchor(t *testing.T) {
	ctl, fw := newTestController(t, windowState{})
	if err := ctl.SetMode(UIModeMini); err != nil {
		t.Fatal(err)
	}
	fw.x = 700
	fw.y = 300 // 中心 300+260 = 560
	ctl.onMoved()
	waitFor(t, "记住锚点", func() bool {
		ctl.opMu.Lock()
		defer ctl.opMu.Unlock()
		return ctl.anchorY == 560
	})
	if st := ctl.State(); st.Collapsed {
		t.Fatal("屏幕中间的拖动不应收起")
	}
	// 面板停在中间时 x/y 都要记住，展开时才能回到原处。
	ctl.opMu.Lock()
	panelCX := ctl.panelCX
	ctl.opMu.Unlock()
	if panelCX != 860 {
		t.Errorf("面板横向锚点 = %d, want 860", panelCX)
	}
}

// 吸边图标被拖到屏幕中间：松手后应吸到最近的一侧，并记住新的纵向位置。
func TestDockDragSnapsToNearestEdge(t *testing.T) {
	ctl, fw := newTestController(t, windowState{})
	if err := ctl.SetMode(UIModeMini); err != nil {
		t.Fatal(err)
	}
	if err := ctl.SetCollapsed(true); err != nil {
		t.Fatal(err)
	}
	if fw.x != 1920-dockSize {
		t.Fatalf("初始应收在右边缘，x = %d", fw.x)
	}

	// 模拟把 64x64 图标拖到屏幕中下方（偏左）。
	fw.x, fw.y = 700, 800
	ctl.onMoved()
	waitFor(t, "吸附到左边缘", func() bool {
		ctl.opMu.Lock()
		defer ctl.opMu.Unlock()
		st := ctl.stateLocked()
		return st.Collapsed && st.Edge == "left" && fw.x == 0 && fw.y == 800
	})
	ctl.Flush()
	st := readStateFile(t, ctl)
	if !st.Collapsed || st.Edge != "left" || st.PanelCX != 1744 || st.AnchorY != 520 || st.DockAnchorY != 832 {
		t.Fatalf("拖动后的位置未落盘: %+v", st)
	}
}

// 吸边图标沿同一条边上下拖动：图标位置自己记；展开面板仍回面板上一次的位置。
func TestDockDragKeepsEdgeAndUpdatesAnchor(t *testing.T) {
	ctl, fw := newTestController(t, windowState{})
	if err := ctl.SetMode(UIModeMini); err != nil {
		t.Fatal(err)
	}
	if err := ctl.SetCollapsed(true); err != nil {
		t.Fatal(err)
	}
	// 收起时图标顶边对齐面板顶边：面板在 y=260，图标也在 y=260。
	if fw.y != 260 {
		t.Fatalf("收起后图标应对齐面板顶边，y = %d", fw.y)
	}
	fw.y = 500 // 仍在右边缘
	ctl.onMoved()
	waitFor(t, "更新图标锚点", func() bool {
		ctl.opMu.Lock()
		defer ctl.opMu.Unlock()
		return ctl.dockAnchorY == 532
	})
	if fw.x != 1920-dockSize {
		t.Fatalf("沿边缘拖动不应换边，x = %d", fw.x)
	}

	// 展开面板：回面板自己的纵向锚点（中心 520），不跟随拖动后的图标（中心 532）。
	if err := ctl.SetCollapsed(false); err != nil {
		t.Fatal(err)
	}
	if fw.y != 520-miniHeight/2 {
		t.Errorf("展开后 y = %d, want %d", fw.y, 520-miniHeight/2)
	}
}

// 拖动过程中按住左键：不要抢先吸附（否则图标会在手里跳走）。
func TestDockDragWaitsForMouseUp(t *testing.T) {
	ctl, fw := newTestController(t, windowState{})
	if err := ctl.SetMode(UIModeMini); err != nil {
		t.Fatal(err)
	}
	if err := ctl.SetCollapsed(true); err != nil {
		t.Fatal(err)
	}
	down := &atomic.Bool{}
	down.Store(true)
	ctl.mouseDown = func() bool { return down.Load() }
	fw.y = 700
	ctl.onMoved()
	time.Sleep(20 * time.Millisecond)
	if fw.y != 700 {
		t.Fatalf("左键未松开时不应吸附，y = %d", fw.y)
	}
	down.Store(false)
	waitFor(t, "松手后吸附", func() bool {
		ctl.opMu.Lock()
		defer ctl.opMu.Unlock()
		return ctl.dockAnchorY == 732
	})
}

// 把图标拖到新位置后立刻点击（位置稳定回调还没跑）：展开要回面板自己的 x/y，
// 只把边和图标自己的锚点更新掉；面板的横向位置不跟图标走。
func TestExpandRestoresPanelPosition(t *testing.T) {
	ctl, fw := newTestController(t, windowState{})
	if err := ctl.SetMode(UIModeMini); err != nil {
		t.Fatal(err)
	}
	if err := ctl.SetCollapsed(true); err != nil {
		t.Fatal(err)
	}
	fw.x, fw.y = 3, 500 // 拖到左边缘、中上位置；不触发 onMoved
	if err := ctl.SetCollapsed(false); err != nil {
		t.Fatal(err)
	}
	// 面板原来在右边缘（1584,260），按记住的 x/y 恢复，不因图标被拖到左边而换边。
	if fw.x != 1920-miniWidth-miniEdgeGap || fw.y != 260 {
		t.Errorf("展开位置 = (%d,%d)，want (%d,%d)", fw.x, fw.y, 1920-miniWidth-miniEdgeGap, 260)
	}
	ctl.Flush()
	st := readStateFile(t, ctl)
	if st.Edge != "left" || st.PanelCX != 1744 || st.AnchorY != 520 || st.DockAnchorY != 532 || st.Collapsed {
		t.Fatalf("展开后的状态不符: %+v", st)
	}
}

// 面板拖到屏幕中间后手动点"收起"：面板的 x、y 都要记住，展开按原位置恢复。
func TestManualCollapseRemembersPanelXY(t *testing.T) {
	ctl, fw := newTestController(t, windowState{})
	if err := ctl.SetMode(UIModeMini); err != nil {
		t.Fatal(err)
	}
	fw.x, fw.y = 500, 300 // 模拟把面板拖到屏幕中间（还没走位置去抖）
	if err := ctl.SetCollapsed(true); err != nil {
		t.Fatal(err)
	}
	// 图标按最近边收起、顶边对齐面板顶边：面板中心 x=660 → 左边缘，图标 y=300。
	if st := ctl.State(); !st.Collapsed || st.Edge != "left" {
		t.Fatalf("手动收起状态不符: %+v", st)
	}
	if fw.x != 0 || fw.y != 300 {
		t.Fatalf("收起后图标位置 = (%d,%d)", fw.x, fw.y)
	}

	if err := ctl.SetCollapsed(false); err != nil {
		t.Fatal(err)
	}
	if fw.x != 500 || fw.y != 300 {
		t.Errorf("展开后应回到面板原位置 (500,300)，实际 (%d,%d)", fw.x, fw.y)
	}
	ctl.Flush()
	st := readStateFile(t, ctl)
	if st.PanelCX != 660 || st.AnchorY != 560 || st.Collapsed {
		t.Fatalf("手动收起后的面板位置未落盘: %+v", st)
	}
}

// 面板被拖到完全贴边后收起：展开时按原 x 恢复（贴边），guard 结束后不能被
// "位置稳定再判吸边"立刻又收起来。
func TestExpandFlushPanelStaysOpen(t *testing.T) {
	ctl, fw := newTestController(t, windowState{})
	ctl.guardWindow = 40 * time.Millisecond
	if err := ctl.SetMode(UIModeMini); err != nil {
		t.Fatal(err)
	}
	fw.x = 1920 - miniWidth // 面板贴住右边缘（没有 16px 缝）
	fw.y = 300
	if err := ctl.SetCollapsed(true); err != nil {
		t.Fatal(err)
	}
	if err := ctl.SetCollapsed(false); err != nil {
		t.Fatal(err)
	}
	if fw.x != 1920-miniWidth || fw.y != 300 {
		t.Fatalf("贴边面板未按原位置展开: (%d,%d)", fw.x, fw.y)
	}
	ctl.onMoved() // 模拟 SetPosition 触发的移动回调
	time.Sleep(120 * time.Millisecond)
	if st := ctl.State(); st.Collapsed {
		t.Fatal("刚展开的贴边面板不应被 guard 后的回调再收起")
	}
}

// 收起后 guard 时间窗内就拖动图标：不能把这次移动直接丢掉，guard 结束后仍要吸附。
func TestDockDragWithinGuardStillSnaps(t *testing.T) {
	ctl, fw := newTestController(t, windowState{})
	ctl.guardWindow = 80 * time.Millisecond
	if err := ctl.SetMode(UIModeMini); err != nil {
		t.Fatal(err)
	}
	if err := ctl.SetCollapsed(true); err != nil {
		t.Fatal(err)
	}
	fw.x, fw.y = 700, 800
	ctl.onMoved()
	waitFor(t, "guard 结束后吸附", func() bool {
		ctl.opMu.Lock()
		defer ctl.opMu.Unlock()
		return fw.x == 0 && fw.y == 800
	})
}

// 拖动入口只在"已收起为图标"时可用；假窗口没有原生句柄，必须报错而不是静默成功
// （前端据此把这次手势退回点击语义）。
func TestBeginDragOnlyForDockedIcon(t *testing.T) {
	ctl, _ := newTestController(t, windowState{})
	if err := ctl.BeginDrag(); err == nil {
		t.Fatal("标准界面不应允许拖动吸边图标")
	}
	if err := ctl.SetMode(UIModeMini); err != nil {
		t.Fatal(err)
	}
	if err := ctl.BeginDrag(); err == nil {
		t.Fatal("展开面板不应允许用吸边图标的拖动入口")
	}
	if err := ctl.SetCollapsed(true); err != nil {
		t.Fatal(err)
	}
	if err := ctl.BeginDrag(); err == nil {
		t.Fatal("没有原生句柄时应报错")
	}
}

func TestCollapsedStateRestored(t *testing.T) {
	st := windowState{
		Mode: UIModeMini, Collapsed: true, Edge: "left",
		PanelCX: 660, AnchorY: 520, DockAnchorY: 700,
	}
	ctl, fw := newTestController(t, st)
	// 真实建窗时窗口已经在吸边位置（app.go 的 newWindowBootstrap）；测试里摆成同样几何。
	fw.w, fw.h = dockSize, dockSize
	fw.x, fw.y = 0, 700-dockSize/2
	if got := ctl.State(); !got.Collapsed || got.Edge != "left" {
		t.Fatalf("控制器未恢复收起状态: %+v", got)
	}
	// 展开后回到面板自己的 x/y（中心 660,520），不是图标的中心 700，也不贴边。
	if err := ctl.SetCollapsed(false); err != nil {
		t.Fatal(err)
	}
	if fw.w != miniWidth || fw.h != miniHeight {
		t.Errorf("展开后尺寸 = %dx%d", fw.w, fw.h)
	}
	if fw.x != 660-miniWidth/2 || fw.y != 520-miniHeight/2 {
		t.Errorf("展开位置 = (%d,%d)，want (%d,%d)", fw.x, fw.y, 660-miniWidth/2, 520-miniHeight/2)
	}
}

func TestWindowBootstrap(t *testing.T) {
	screens := func() []*application.Screen { return []*application.Screen{testPrimaryScreen} }
	pinned := true

	// 标准界面：尺寸来自状态文件，居中，带边框。
	got := newWindowBootstrap(screens, windowState{Width: 1000, Height: 700})
	if got.width != 1000 || got.height != 700 || got.frameless || got.position != application.WindowCentered {
		t.Fatalf("标准界面建窗参数不符: %+v", got)
	}

	// 没有横向记录的旧状态：贴右边缘留 16 DIP。
	got = newWindowBootstrap(screens, windowState{Width: 1000, Height: 700, Mode: UIModeMini, Edge: "right", AnchorY: 520})
	if got.width != miniWidth || got.height != miniHeight || !got.frameless || !got.disableResize {
		t.Fatalf("简约面板建窗参数不符: %+v", got)
	}
	if got.x != 1920-miniWidth-miniEdgeGap || got.y != 520-miniHeight/2 {
		t.Fatalf("简约面板位置 = (%d,%d)", got.x, got.y)
	}
	if got.alwaysOnTop {
		t.Error("没有置顶偏好时简约面板不应置顶")
	}

	// 记住过面板 x/y（可能停在屏幕中间）：按记录恢复，不贴边。
	got = newWindowBootstrap(screens, windowState{
		Width: 1000, Height: 700, Mode: UIModeMini,
		Edge: "right", PanelCX: 660, AnchorY: 520,
	})
	if got.x != 660-miniWidth/2 || got.y != 520-miniHeight/2 {
		t.Fatalf("简约面板应回到记录位置，实际 (%d,%d)", got.x, got.y)
	}

	// 上次停在吸边图标：64x64、置顶（即使没点过置顶），落在同一侧。
	// 没有图标锚点时按"图标顶边对齐面板顶边"推算：面板在 y=440，图标也在 y=440。
	got = newWindowBootstrap(screens, windowState{
		Width: 1000, Height: 700, Mode: UIModeMini, Collapsed: true,
		Edge: "left", AnchorY: 700, Pinned: &pinned,
	})
	if got.width != dockSize || got.height != dockSize || got.minWidth != dockSize || got.maxWidth != dockSize {
		t.Fatalf("吸边图标建窗尺寸不符: %+v", got)
	}
	if got.x != 0 || got.y != 700-miniHeight/2 {
		t.Fatalf("吸边图标位置 = (%d,%d)", got.x, got.y)
	}
	if !got.alwaysOnTop {
		t.Error("吸边图标必须置顶")
	}

	// 有图标自己的锚点时按它恢复（拖动过的图标位置），不跟面板位置混用。
	got = newWindowBootstrap(screens, windowState{
		Width: 1000, Height: 700, Mode: UIModeMini, Collapsed: true,
		Edge: "right", AnchorY: 300, DockAnchorY: 800,
	})
	if got.x != 1920-dockSize || got.y != 800-dockSize/2 {
		t.Fatalf("吸边图标位置 = (%d,%d)", got.x, got.y)
	}

	// 没有屏幕信息时退回默认定位，但几何仍按收起状态。
	got = newWindowBootstrap(func() []*application.Screen { return nil }, windowState{Mode: UIModeMini, Collapsed: true})
	if got.width != dockSize || got.position != application.WindowCentered {
		t.Fatalf("无屏幕信息时建窗参数不符: %+v", got)
	}
}

// 直接双击启动时，Wails 用带标题栏的样式建 64x64 窗口，系统会把它撑开/挪位；
// 去掉样式位后必须重新套一遍几何，否则吸边图标只露出一点点。
func TestApplyNativeStyleReappliesDockGeometry(t *testing.T) {
	st := windowState{
		Mode: UIModeMini, Collapsed: true, Edge: "right",
		PanelCX: 1744, AnchorY: 520, DockAnchorY: 488,
		PanelScreen: testPrimaryScreen.Name, DockScreen: testPrimaryScreen.Name,
	}
	ctl, fw := newTestController(t, st)
	fw.native = 1        // 模拟原生句柄已就绪（否则 ApplyNativeStyle 会等句柄）
	fw.w, fw.h = 131, 52 // 标题栏窗口被系统撑开后的错误几何
	fw.x, fw.y = 1856, 0
	ctl.ApplyNativeStyle()
	if style, ok := ctl.style.(*fakeStyle); !ok || !style.mini {
		t.Fatalf("未切换到吸边样式: %#v", ctl.style)
	}
	if fw.w != dockSize || fw.h != dockSize {
		t.Fatalf("窗口尺寸未被修正: %dx%d", fw.w, fw.h)
	}
	if fw.x != 1920-dockSize || fw.y != 488-dockSize/2 {
		t.Fatalf("窗口位置未被修正: (%d,%d)", fw.x, fw.y)
	}
}

func TestApplyNativeStyleReappliesMiniGeometry(t *testing.T) {
	st := windowState{
		Mode: UIModeMini, Edge: "right",
		PanelCX: 660, AnchorY: 520, PanelScreen: testPrimaryScreen.Name,
	}
	ctl, fw := newTestController(t, st)
	fw.native = 1
	fw.w, fw.h = 131, 52
	fw.x, fw.y = 0, 0
	ctl.ApplyNativeStyle()
	if fw.w != miniWidth || fw.h != miniHeight {
		t.Fatalf("面板尺寸未被修正: %dx%d", fw.w, fw.h)
	}
	if fw.x != 660-miniWidth/2 || fw.y != 520-miniHeight/2 {
		t.Fatalf("面板位置未被修正: (%d,%d)", fw.x, fw.y)
	}
}

// 副屏上的面板/图标位置：建窗时按屏幕名恢复，旧状态按绝对坐标反查屏幕。
func TestBootstrapSecondaryScreen(t *testing.T) {
	got := newWindowBootstrap(testScreensTwo, windowState{
		Width: 1000, Height: 700, Mode: UIModeMini,
		PanelCX: 2560, AnchorY: 500, PanelScreen: testSecondScreen.Name,
	})
	if got.x != 2560-miniWidth/2 || got.y != 500-miniHeight/2 {
		t.Fatalf("副屏面板建窗位置 = (%d,%d)", got.x, got.y)
	}

	// 旧状态没有屏幕名：按绝对坐标反查副屏，不能夹回主屏。
	got = newWindowBootstrap(testScreensTwo, windowState{
		Width: 1000, Height: 700, Mode: UIModeMini,
		PanelCX: 2560, AnchorY: 500,
	})
	if got.x != 2560-miniWidth/2 || got.y != 500-miniHeight/2 {
		t.Fatalf("旧状态副屏面板位置 = (%d,%d)", got.x, got.y)
	}

	// 收起的图标在副屏右边缘。
	got = newWindowBootstrap(testScreensTwo, windowState{
		Width: 1000, Height: 700, Mode: UIModeMini, Collapsed: true, Edge: "right",
		PanelCX: 2560, AnchorY: 500, PanelScreen: testSecondScreen.Name,
		DockAnchorY: 600, DockScreen: testSecondScreen.Name,
	})
	if got.x != 1920+1280-dockSize || got.y != 600-dockSize/2 {
		t.Fatalf("副屏图标建窗位置 = (%d,%d)", got.x, got.y)
	}
}

// 在副屏上拖动/收起/展开：屏幕名和坐标都要落盘，展开按副屏位置恢复。
func TestSecondaryScreenPositionPersists(t *testing.T) {
	ctl, fw := newTestController(t, windowState{})
	ctl.screens = testScreensTwo
	fw.screen = testSecondScreen

	if err := ctl.SetMode(UIModeMini); err != nil {
		t.Fatal(err)
	}
	// 首次进入简约界面：面板贴副屏右边缘（1920+1280-320-16）。
	if want := 1920 + 1280 - miniWidth - miniEdgeGap; fw.x != want {
		t.Fatalf("副屏面板 x = %d, want %d", fw.x, want)
	}
	if err := ctl.SetCollapsed(true); err != nil {
		t.Fatal(err)
	}
	// 图标贴副屏右边缘、顶边对齐面板顶边。
	if fw.x != 1920+1280-dockSize || fw.y != 260 {
		t.Fatalf("副屏图标位置 = (%d,%d)", fw.x, fw.y)
	}

	// 在副屏上把图标拖到左边缘附近。
	fw.x, fw.y = 2500, 600
	ctl.onMoved()
	waitFor(t, "副屏图标吸附", func() bool {
		ctl.opMu.Lock()
		defer ctl.opMu.Unlock()
		return fw.x == 1920 && fw.y == 600 && ctl.dockScreen == testSecondScreen.Name
	})

	// 展开：面板回它自己在副屏上的位置（面板右边缘留缝），不贴图标。
	if err := ctl.SetCollapsed(false); err != nil {
		t.Fatal(err)
	}
	if fw.x != 1920+1280-miniWidth-miniEdgeGap || fw.y != 260 {
		t.Fatalf("展开位置 = (%d,%d)", fw.x, fw.y)
	}
	ctl.Flush()
	st := readStateFile(t, ctl)
	if st.PanelScreen != testSecondScreen.Name || st.DockScreen != testSecondScreen.Name {
		t.Fatalf("副屏屏幕名未落盘: %+v", st)
	}
	if st.PanelCX != 1920+1280-miniWidth-miniEdgeGap+miniWidth/2 || st.AnchorY != 520 {
		t.Fatalf("副屏面板坐标未落盘: %+v", st)
	}
	if st.DockAnchorY != 632 {
		t.Fatalf("副屏图标锚点未落盘: %+v", st)
	}
}

func TestGuardIgnoresProgrammaticMove(t *testing.T) {
	ctl, fw := newTestController(t, windowState{})
	ctl.guardWindow = 300 * time.Millisecond
	if err := ctl.SetMode(UIModeMini); err != nil {
		t.Fatal(err)
	}
	// 切模式的程序化摆放把面板放在贴边位置（留缝）；紧接着的移动回调必须被忽略，
	// 否则刚展开的面板会被立刻收起。
	ctl.onMoved()
	time.Sleep(20 * time.Millisecond)
	if st := ctl.State(); st.Collapsed {
		t.Fatal("程序化摆放触发的移动回调不应收起面板")
	}
	_ = fw
}

func TestResizeTrackingStandardOnly(t *testing.T) {
	ctl, fw := newTestController(t, windowState{})
	fw.w, fw.h = 1280, 840
	ctl.onResized()
	waitFor(t, "记录标准窗口尺寸", func() bool {
		ctl.opMu.Lock()
		defer ctl.opMu.Unlock()
		return ctl.stdW == 1280 && ctl.stdH == 840
	})

	// 简约界面里的程序化改尺寸不应污染标准窗口尺寸。
	if err := ctl.SetMode(UIModeMini); err != nil {
		t.Fatal(err)
	}
	ctl.onResized()
	time.Sleep(10 * time.Millisecond)
	ctl.opMu.Lock()
	stdW, stdH := ctl.stdW, ctl.stdH
	ctl.opMu.Unlock()
	if stdW != 1280 || stdH != 840 {
		t.Fatalf("简约界面的尺寸变化污染了标准尺寸: %dx%d", stdW, stdH)
	}
}

// SetMode 是下次启动的默认界面来源：必须立刻落盘，不能等 400ms 去抖。
func TestSetModeWritesImmediately(t *testing.T) {
	ctl, _ := newTestController(t, windowState{})
	if err := ctl.SetMode(UIModeMini); err != nil {
		t.Fatal(err)
	}
	if st := readStateFile(t, ctl); st.Mode != UIModeMini {
		t.Fatalf("模式切换后应立即落盘: %+v", st)
	}
	ctl.Stop()
}

func TestControllerPersistsMode(t *testing.T) {
	ctl, _ := newTestController(t, windowState{})
	if err := ctl.SetMode(UIModeMini); err != nil {
		t.Fatal(err)
	}
	if err := ctl.SetPinned(true); err != nil {
		t.Fatal(err)
	}
	if err := ctl.SetCollapsed(true); err != nil {
		t.Fatal(err)
	}
	// SetMode 已立即落盘，置顶/收起仍走去抖：这里主动 Flush，验证三者的最终状态。
	ctl.Flush()
	st := readStateFile(t, ctl)
	if st.Mode != UIModeMini || st.Pinned == nil || !*st.Pinned {
		t.Fatalf("落盘状态不符: %+v", st)
	}
	if st.Width != defaultWidth || st.Height != defaultHeight {
		t.Errorf("标准尺寸也应落盘: %+v", st)
	}
	// 收起时图标顶边对齐面板顶边：面板中心 (1744,520) → 面板顶 260 → 图标中心 292。
	if !st.Collapsed || st.PanelCX != 1744 || st.AnchorY != 520 || st.DockAnchorY != 292 {
		t.Errorf("收起状态/锚点未落盘: %+v", st)
	}

	ctl.Stop()
}

func TestStopFlushesAndIgnoresLaterCalls(t *testing.T) {
	ctl, fw := newTestController(t, windowState{})
	if err := ctl.SetMode(UIModeMini); err != nil {
		t.Fatal(err)
	}
	ctl.Stop()
	if st := readStateFile(t, ctl); st.Mode != UIModeMini {
		t.Fatalf("退出时应立刻落盘: %+v", st)
	}

	// 退出后所有入口都应是空操作（定时器可能比窗口晚一步触发）。
	fw.w, fw.h = 111, 222
	if err := ctl.SetMode(UIModeStandard); err != nil {
		t.Fatalf("退出后 SetMode 不应报错: %v", err)
	}
	if fw.w != 111 || fw.h != 222 {
		t.Error("退出后不应再操作窗口")
	}
}

// ---------- Service 绑定层 ----------

type fakeUI struct {
	st        UiState
	modeCalls []string
	pinCalls  []bool
	collapse  []bool
	dragCalls int
	hidden    bool
	err       error
}

func (f *fakeUI) State() UiState { return f.st }

func (f *fakeUI) SetMode(mode string) error {
	f.modeCalls = append(f.modeCalls, mode)
	if f.err != nil {
		return f.err
	}
	f.st.Mode = normalizeUIMode(mode)
	f.st.Collapsed = false
	return nil
}

func (f *fakeUI) SetPinned(on bool) error {
	f.pinCalls = append(f.pinCalls, on)
	if f.err != nil {
		return f.err
	}
	f.st.AlwaysOnTop = on
	return nil
}

func (f *fakeUI) SetCollapsed(v bool) error {
	f.collapse = append(f.collapse, v)
	if f.err != nil {
		return f.err
	}
	f.st.Collapsed = v
	return nil
}

func (f *fakeUI) BeginDrag() error {
	f.dragCalls++
	return f.err
}

func (f *fakeUI) Hide() { f.hidden = true }

func TestServiceUIWithoutController(t *testing.T) {
	s, _, _ := newTestService(t)
	if st := s.UiState(); st.Mode != UIModeStandard {
		t.Fatalf("没有控制器时应回退标准界面: %+v", st)
	}
	if _, err := s.SetUiMode(UIModeMini); err == nil {
		t.Fatal("没有控制器时切换应报错")
	}
	if _, err := s.SetCollapsed(true); err == nil {
		t.Fatal("没有控制器时收起应报错")
	}
	if _, err := s.SetAlwaysOnTop(true); err == nil {
		t.Fatal("没有控制器时置顶应报错")
	}
	if err := s.BeginWindowDrag(); err == nil {
		t.Fatal("没有控制器时拖动应报错")
	}
	if err := s.HideWindow(); err == nil {
		t.Fatal("没有控制器时隐藏窗口应报错")
	}
}

func TestServiceUIBindings(t *testing.T) {
	s, _, _ := newTestService(t)
	ui := &fakeUI{st: UiState{Mode: UIModeStandard}}
	s.setUI(ui)

	st, err := s.SetUiMode(UIModeMini)
	if err != nil || st.Mode != UIModeMini {
		t.Fatalf("SetUiMode = %+v, %v", st, err)
	}
	if _, err := s.SetAlwaysOnTop(true); err != nil {
		t.Fatalf("SetAlwaysOnTop: %v", err)
	}
	st, err = s.SetCollapsed(true)
	if err != nil || !st.Collapsed {
		t.Fatalf("SetCollapsed = %+v, %v", st, err)
	}
	if err := s.BeginWindowDrag(); err != nil || ui.dragCalls != 1 {
		t.Fatalf("BeginWindowDrag: err=%v calls=%d", err, ui.dragCalls)
	}
	if err := s.HideWindow(); err != nil || !ui.hidden {
		t.Fatalf("HideWindow: err=%v hidden=%v", err, ui.hidden)
	}

	// 控制器报错时，错误要原样交给前端（前端据此 toast 原因）。
	ui.err = errors.New("屏幕信息不可用")
	if _, err := s.SetUiMode(UIModeStandard); err == nil {
		t.Fatal("控制器的错误应向上抛")
	}
}
