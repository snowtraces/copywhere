package wailsui

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// 窗口控制器的时间参数。
const (
	// miniGuard：程序化调整窗口（切模式/吸边/展开）后忽略"位置稳定"回调的时间窗，
	// 避免自己挪窗口又触发一次吸边收起。
	miniGuard = 700 * time.Millisecond
	// moveSettleDelay：拖动事件停下多久算"位置稳定"。Wails 在 Windows 上
	// 已按 50ms 对 WM_MOVE 去抖，这里再等一会儿，避开拖动过程中的中间位置。
	moveSettleDelay = 220 * time.Millisecond
	// windowStateSaveDelay：尺寸/位置落盘去抖。
	windowStateSaveDelay = 400 * time.Millisecond
)

// uiWindow 是窗口控制器需要的窗口能力。
// *application.WebviewWindow 的方法签名带返回值，这里用 wailsWindow 适配一层；
// 单测直接用假实现，几何与状态机不依赖真正的 Win32 窗口。
type uiWindow interface {
	Size() (int, int)
	Position() (int, int)
	SetSize(width, height int)
	SetPosition(x, y int)
	SetMinSize(width, height int)
	SetMaxSize(width, height int)
	SetResizable(resizable bool)
	SetFrameless(frameless bool)
	SetAlwaysOnTop(alwaysOnTop bool)
	GetScreen() (*application.Screen, error)
	// NativeWindow 返回原生窗口句柄；测试与非 Windows 实现返回 0。
	NativeWindow() uintptr
	Show()
	Focus()
	Hide()
}

// wailsWindow 把 *application.WebviewWindow 适配成 uiWindow。
type wailsWindow struct{ win *application.WebviewWindow }

func (w wailsWindow) Size() (int, int)          { return w.win.Size() }
func (w wailsWindow) Position() (int, int)      { return w.win.Position() }
func (w wailsWindow) SetSize(width, height int) { w.win.SetSize(width, height) }
func (w wailsWindow) SetPosition(x, y int)      { w.win.SetPosition(x, y) }
func (w wailsWindow) SetMinSize(width, height int) {
	w.win.SetMinSize(width, height)
}
func (w wailsWindow) SetMaxSize(width, height int) {
	w.win.SetMaxSize(width, height)
}
func (w wailsWindow) SetResizable(resizable bool)     { w.win.SetResizable(resizable) }
func (w wailsWindow) SetFrameless(frameless bool)     { w.win.SetFrameless(frameless) }
func (w wailsWindow) SetAlwaysOnTop(alwaysOnTop bool) { w.win.SetAlwaysOnTop(alwaysOnTop) }
func (w wailsWindow) GetScreen() (*application.Screen, error) {
	return w.win.GetScreen()
}
func (w wailsWindow) NativeWindow() uintptr {
	if w.win == nil {
		return 0
	}
	return uintptr(w.win.NativeWindow())
}
func (w wailsWindow) Show()  { w.win.Show() }
func (w wailsWindow) Focus() { w.win.Focus() }
func (w wailsWindow) Hide()  { w.win.Hide() }

// windowStyler 是窗口外观的原生增强：吸边图标必须去掉标题栏样式位，否则 Windows
// 会把窗口宽度撑到约 131px；其它平台是空实现。
type windowStyler interface {
	setMini(on bool)
}

// nativeStyler 用窗口原生句柄实现 windowStyler。
type nativeStyler struct{ win uiWindow }

func (s nativeStyler) setMini(on bool) {
	hwnd := s.win.NativeWindow()
	if hwnd == 0 {
		return
	}
	if on {
		setFramelessDockStyle(hwnd)
		return
	}
	restoreCaptionStyle(hwnd)
}

// windowState 是 wails-window.json 的内容：标准界面尺寸 + 简约界面偏好。
// 简体旧版本只写了 width/height，读取时对缺省字段做归一化，不会破坏已有文件。
type windowState struct {
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Mode   string `json:"mode,omitempty"`
	Pinned *bool  `json:"pinned,omitempty"` // nil = 从未设置过（默认不置顶）
	Edge   string `json:"edge,omitempty"`
	// AnchorY 是简约面板的纵向锚点（面板中心，DIP），展开时按它恢复上次位置。
	AnchorY int `json:"anchorY,omitempty"`
	// PanelCX 是简约面板的横向锚点（面板中心，DIP），0 = 没记录（按吸边侧摆放）。
	// 面板允许停在屏幕任意位置，x/y 都要记住；手动点收起也走同一份记录。
	PanelCX int `json:"panelCX,omitempty"`
	// PanelScreen / DockScreen 是面板/吸边图标所在显示器的 Name（Windows 下为
	// "\\.\DISPLAYn"）。Wails 的 Screen.ID 是 HMONITOR，重启后会变，不能用来跨会话
	// 定位；Name 稳定。副屏上的坐标必须配合它才能在下次启动时回到同一块屏。
	PanelScreen string `json:"panelScreen,omitempty"`
	DockScreen  string `json:"dockScreen,omitempty"`
	// DockAnchorY 是吸边图标的纵向锚点（图标中心，DIP），0 = 没记录
	// （按"图标顶边对齐面板顶边"推算）。它与 AnchorY 分开记忆：
	// 拖动图标只改图标自己的位置，展开面板仍回面板上一次的位置。
	DockAnchorY int  `json:"dockAnchorY,omitempty"`
	Collapsed   bool `json:"collapsed,omitempty"` // 上次退出时收在边缘（下次按 64x64 图标建窗）
}

// windowStatePath 返回窗口状态文件（与配置文件同目录）。
func windowStatePath(cfgPath string) string {
	dir := filepath.Dir(cfgPath)
	if cfgPath == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".copywhere")
	}
	return filepath.Join(dir, "wails-window.json")
}

// readWindowState 读取窗口状态；损坏或越界时回退到默认值。
func readWindowState(path string) windowState {
	var st windowState
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &st); err != nil {
			log.Printf("窗口状态文件损坏，改用默认值: %v", err)
			st = windowState{}
		}
	}
	if st.Width < minWidth || st.Width > 6000 {
		st.Width = defaultWidth
	}
	if st.Height < minHeight || st.Height > 4000 {
		st.Height = defaultHeight
	}
	st.Mode = normalizeUIMode(st.Mode)
	if st.Edge == "" {
		st.Edge = "right"
	}
	st.Edge = normalizeEdge(st.Edge)
	// 收起状态只对简约界面有意义：标准界面里残留的 true 会让下次建窗时
	// 误以为要按 64x64 图标建。
	if st.Mode != UIModeMini {
		st.Collapsed = false
	}
	return st
}

// windowStateWrite 记录窗口状态：下次启动恢复尺寸与简约界面偏好。
func windowStateWrite(path string, st windowState) {
	data, err := json.Marshal(st)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		log.Printf("记录窗口状态失败: %v", err)
	}
}

// windowController 负责"标准界面 ↔ 简约界面"的窗口几何、置顶与吸边收起。
//
// 并发约定：opMu 保护全部字段，并且允许在持锁期间调用窗口 API。
// 所有入口都运行在独立 goroutine（Wails 服务调用、窗口事件、定时器），
// 主线程通常不会获取这把锁，因此 InvokeSync 不会与它形成环。
// 例外：退出时 Wails 在主线程执行 OnShutdown（ctl.Stop 会拿这把锁），
// 所以需要等主线程的操作（如 BeginDrag 的 InvokeSync）必须先把锁放掉。
type windowController struct {
	win      uiWindow
	style    windowStyler
	path     string
	screens  func() []*application.Screen
	onChange func(UiState)

	opMu sync.Mutex

	mode      string
	collapsed bool
	edge      string
	pinned    bool
	// anchorY：面板的纵向锚点（面板中心）。展开时按它恢复上一次窗口位置。
	anchorY int
	// panelCX：面板的横向锚点（面板中心）。0 = 还没记录（首次进入简约界面时按吸边侧摆放）。
	panelCX int
	// panelScreen / dockScreen：面板/图标所在显示器的 Name；副屏坐标靠它恢复。
	panelScreen string
	dockScreen  string
	// dockAnchorY：吸边图标的纵向锚点（图标中心）。拖动图标只改它；
	// 收起时图标顶边对齐面板顶边，因此它通常等于面板顶边 + dockSize/2。
	dockAnchorY int
	stdW        int
	stdH        int

	// 三个时间参数抽成字段，单测可以调到毫秒级，不必真等。
	settleDelay time.Duration
	guardWindow time.Duration
	saveDelay   time.Duration
	// mouseDown 判断左键是否仍按着（拖动未结束）；单测里固定为 false。
	mouseDown func() bool

	guardUntil time.Time
	moveTimer  *time.Timer
	saveTimer  *time.Timer
	dirty      bool
	stopped    bool
}

// newWindowController 依据已读取的窗口状态创建控制器。
// 窗口本身的初始尺寸/边框由 app.go 在建窗时按同一份状态设置好。
func newWindowController(win uiWindow, style windowStyler, path string, st windowState, screens func() []*application.Screen) *windowController {
	pinned := false
	if st.Pinned != nil {
		pinned = *st.Pinned
	}
	mode := normalizeUIMode(st.Mode)
	return &windowController{
		win:         win,
		style:       style,
		path:        path,
		screens:     screens,
		mode:        mode,
		collapsed:   mode == UIModeMini && st.Collapsed,
		edge:        normalizeEdge(st.Edge),
		pinned:      pinned,
		anchorY:     st.AnchorY,
		panelCX:     st.PanelCX,
		panelScreen: st.PanelScreen,
		dockScreen:  st.DockScreen,
		dockAnchorY: st.DockAnchorY,
		stdW:        st.Width,
		stdH:        st.Height,
		settleDelay: moveSettleDelay,
		guardWindow: miniGuard,
		saveDelay:   windowStateSaveDelay,
		mouseDown:   leftMouseDown,
	}
}

// setOnChange 注入状态变化观察者（推前端事件 + 同步托盘勾选）。
func (c *windowController) setOnChange(fn func(UiState)) {
	c.opMu.Lock()
	c.onChange = fn
	c.opMu.Unlock()
}

// State 返回当前界面状态快照。
func (c *windowController) State() UiState {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	return c.stateLocked()
}

func (c *windowController) stateLocked() UiState {
	w, h := c.win.Size()
	return UiState{
		Mode:        c.mode,
		Collapsed:   c.collapsed,
		Edge:        normalizeEdge(c.edge),
		AlwaysOnTop: c.effectiveTopLocked(),
		Width:       w,
		Height:      h,
	}
}

// effectiveTopLocked：收起成边缘图标时强制置顶，保证图标不被其它窗口盖住；
// 展开后回到用户选择的置顶开关（托盘与简约面板上的「置顶」按钮）。
func (c *windowController) effectiveTopLocked() bool {
	if c.mode != UIModeMini {
		return false
	}
	return c.pinned || c.collapsed
}

// SetMode 在标准界面与简约界面之间切换。
func (c *windowController) SetMode(mode string) error {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode != UIModeStandard && mode != UIModeMini {
		return fmt.Errorf("未知界面模式: %q", mode)
	}
	c.opMu.Lock()
	defer c.opMu.Unlock()
	if c.stopped || mode == c.mode {
		return nil
	}
	work, ok := c.workAreaLocked()
	if !ok {
		return errors.New("无法获取窗口所在屏幕的工作区")
	}
	if mode == UIModeMini {
		// 记下标准窗口的尺寸，下一次切回来原样恢复。
		if w, h := c.win.Size(); w >= minWidth && h >= minHeight {
			c.stdW, c.stdH = w, h
		}
		if c.edge == "" {
			c.edge = nearestEdge(work, c.rectLocked())
		}
		// 已经用过简约界面：保留上次的面板位置（x/y）；首次进入才用当前窗口中心。
		if c.anchorY <= 0 {
			c.anchorY = c.rectLocked().centerY()
		}
		c.mode = UIModeMini
		c.collapsed = false
		c.applyMiniLocked()
	} else {
		c.mode = UIModeStandard
		c.collapsed = false
		c.applyStandardLocked(work)
	}
	c.guardLocked()
	// 模式切换次数少、又是下次启动的默认界面：立刻落盘，不等 400ms 去抖——
	// 用户切到简约界面后马上退出/被结束进程，也要能记住这一次选择。
	c.writeStateLocked()
	c.changedLocked()
	return nil
}

// SetPinned 切换「固定在屏幕最上层」。仅在简约界面生效，但偏好会被记住。
func (c *windowController) SetPinned(on bool) error {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	if c.stopped {
		return nil
	}
	c.pinned = on
	if c.mode == UIModeMini {
		c.win.SetAlwaysOnTop(c.effectiveTopLocked())
	}
	c.persistLocked()
	c.changedLocked()
	return nil
}

// SetCollapsed 收起为屏幕边缘的小图标 / 从图标展开。
func (c *windowController) SetCollapsed(collapsed bool) error {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	if c.stopped {
		return nil
	}
	if c.mode != UIModeMini {
		return errors.New("吸边收起只在简约界面可用")
	}
	if collapsed == c.collapsed {
		return nil
	}
	work, ok := c.workAreaLocked()
	if !ok {
		return errors.New("无法获取窗口所在屏幕的工作区")
	}
	if collapsed {
		win := c.rectLocked()
		edge := edgeHit(work, win, edgeSnapTol)
		if edge == "" {
			edge = nearestEdge(work, win)
		}
		c.edge = edge
		c.collapseLocked(work, win)
	} else {
		// 展开前先认一下图标的真实位置：用户可能刚把图标拖到新位置就点了它，
		// 220ms 的位置去抖还没跑。图标自己的锚点与展开位置无关（面板回它自己的
		// 上一次位置），但边要跟着图标走，拖动过的那次位置也别丢。
		icon := c.rectLocked()
		c.edge = nearestEdge(work, icon)
		c.dockAnchorY = icon.centerY()
		if sc := c.currentScreenLocked(); sc != nil {
			c.dockScreen = sc.Name
		}
		c.expandLocked()
	}
	c.guardLocked()
	c.persistLocked()
	c.changedLocked()
	return nil
}

// Reveal 保证窗口以"看得见的面板"出现：收起到边缘时先展开。
func (c *windowController) Reveal() {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	if c.stopped || c.mode != UIModeMini || !c.collapsed {
		return
	}
	c.expandLocked()
	c.guardLocked()
	c.persistLocked()
	c.changedLocked()
}

// BeginDrag 让吸边图标进入系统的原生拖动循环。
//
// 为什么不在图标上用 CSS 的 --wails-draggable: drag：那样拖动与点击会争抢同一次
// 鼠标手势（Wails 在指针一移动时就进入拖动并吞掉 click），图标就时灵时不灵。
// 这里由前端判定"按住移动超过阈值"后再调用本方法，点击与拖动各自可靠；
// 松手后的位置交给 onMoved/settleMoved 吸到最近的边缘并落盘。
func (c *windowController) BeginDrag() error {
	c.opMu.Lock()
	stopped := c.stopped
	ok := c.mode == UIModeMini && c.collapsed
	hwnd := c.win.NativeWindow()
	c.opMu.Unlock()
	switch {
	case stopped:
		return nil
	case !ok:
		return errors.New("只有吸边图标可以这样拖动")
	case hwnd == 0:
		return errors.New("窗口尚未就绪")
	}
	var err error
	// 不持锁调用 InvokeSync：退出时 Wails 会在主线程执行 OnShutdown（ctl.Stop 要拿
	// 这把锁），持锁等主线程会死锁。句柄在锁外使用也没关系，窗口已销毁时
	// PostMessage 只会失败并返回错误。
	//
	// 必须在 UI 线程执行：ReleaseCapture 释放的是"当前线程"的鼠标捕获，
	// 而绑定方法运行在 Wails 的工作 goroutine 上。
	application.InvokeSync(func() { err = startWindowDrag(hwnd) })
	return err
}

// Hide 把窗口收进托盘（进程与同步继续）。
func (c *windowController) Hide() {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	if c.stopped {
		return
	}
	c.win.Hide()
}

// Stop 在退出前停掉定时器并落盘；之后所有入口都变成空操作。
func (c *windowController) Stop() {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	if c.stopped {
		return
	}
	c.stopped = true
	if c.moveTimer != nil {
		c.moveTimer.Stop()
		c.moveTimer = nil
	}
	if c.saveTimer != nil {
		c.saveTimer.Stop()
		c.saveTimer = nil
	}
	c.writeStateLocked()
}

// Flush 立刻把当前状态落盘（关闭到托盘时调用，避免去抖窗口里丢掉最后一次改动）。
func (c *windowController) Flush() {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	if c.stopped {
		return
	}
	if c.saveTimer != nil {
		c.saveTimer.Stop()
		c.saveTimer = nil
	}
	c.writeStateLocked()
}

// onResized 记录用户手动缩放后的标准窗口尺寸（简约界面里的程序化改尺寸不算）。
func (c *windowController) onResized() {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	if c.stopped || c.mode != UIModeStandard {
		return
	}
	w, h := c.win.Size()
	if w < minWidth || h < minHeight {
		return
	}
	if w == c.stdW && h == c.stdH {
		return
	}
	c.stdW, c.stdH = w, h
	c.persistLocked()
}

// onMoved 由窗口移动事件触发：启动去抖，位置稳定后再判断是否吸边。
// 收起成图标时同样需要处理：拖动图标改变纵向位置（或换边）后要吸回边缘并记住。
func (c *windowController) onMoved() {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	if c.stopped || c.mode != UIModeMini {
		return
	}
	if c.moveTimer != nil {
		c.moveTimer.Stop()
	}
	c.moveTimer = time.AfterFunc(c.settleDelay, c.settleMoved)
}

// settleMoved 在拖动停下来后执行：面板贴近左右边缘就吸边收起，否则只记住锚点；
// 已经是吸边图标时，吸附到最近的一侧并记录新的纵向位置。
func (c *windowController) settleMoved() {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	c.moveTimer = nil
	if c.stopped || c.mode != UIModeMini {
		return
	}
	// 刚由程序摆好窗口（切模式/收起/展开）：等 guard 结束再看一次，而不是直接
	// 丢掉这次位置。用户可能在 guard 时间窗内就开始拖吸边图标，丢掉会让图标停在
	// 半路、锚点也没更新。只对收起态重试：展开的面板允许被恢复到贴边位置（x/y
	// 原样记忆），guard 结束后再判一次吸边会把它刚展开就又收起来。
	if wait := time.Until(c.guardUntil); wait > 0 {
		if c.collapsed {
			c.moveTimer = time.AfterFunc(wait, c.settleMoved)
		}
		return
	}
	// 用户还按着左键（正拖到一半）：不要抢先吸边收起，等松手后再判断。
	if c.mouseDown != nil && c.mouseDown() {
		c.moveTimer = time.AfterFunc(c.settleDelay, c.settleMoved)
		return
	}
	work, ok := c.workAreaLocked()
	if !ok {
		return
	}
	win := c.rectLocked()
	if c.collapsed {
		c.settleDockedLocked(work, win)
		return
	}
	if edge := edgeHit(work, win, edgeSnapTol); edge != "" {
		c.edge = edge
		c.collapseLocked(work, win)
		c.guardLocked()
		c.persistLocked()
		c.changedLocked()
		return
	}
	// 面板停在屏幕中间：记住完整的 x/y 和所在屏幕，下次展开/重启都按它恢复。
	screen := ""
	if sc := c.currentScreenLocked(); sc != nil {
		screen = sc.Name
	}
	if c.anchorY != win.centerY() || c.panelCX != win.centerX() || c.panelScreen != screen {
		c.anchorY = win.centerY()
		c.panelCX = win.centerX()
		if screen != "" {
			c.panelScreen = screen
		}
		c.persistLocked()
	}
}

// settleDockedLocked 把拖动后的吸边图标吸附到最近的工作区左/右边缘，
// 并记住图标自己的纵向锚点与所在屏幕。面板的锚点保持不变——点击图标展开时
// 面板回它上一次的位置，而不是跟着图标走。
func (c *windowController) settleDockedLocked(work, win uiRect) {
	edge := nearestEdge(work, win)
	target := dockedRect(work, dockSize, win.centerY(), edge)
	screen := ""
	if sc := c.currentScreenLocked(); sc != nil {
		screen = sc.Name
	}
	changed := edge != c.edge || target.centerY() != c.dockAnchorY || (screen != "" && screen != c.dockScreen)
	c.edge = edge
	c.dockAnchorY = target.centerY()
	if screen != "" {
		c.dockScreen = screen
	}
	if win.X != target.X || win.Y != target.Y {
		c.win.SetPosition(target.X, target.Y)
		c.guardLocked()
		changed = true
	}
	if changed {
		c.persistLocked()
		c.changedLocked()
	}
}

// ---------- 几何应用（全部要求已持锁） ----------

func (c *windowController) rectLocked() uiRect {
	x, y := c.win.Position()
	w, h := c.win.Size()
	return uiRect{X: x, Y: y, W: w, H: h}
}

// currentScreenLocked 返回窗口当前所在屏幕；取不到时退回主屏。
func (c *windowController) currentScreenLocked() *application.Screen {
	if sc, err := c.win.GetScreen(); err == nil && sc != nil {
		return sc
	}
	if c.screens != nil {
		return screensPrimary(c.screens())
	}
	return nil
}

// workAreaLocked 返回窗口当前所在屏幕的工作区（DIP）。
func (c *windowController) workAreaLocked() (uiRect, bool) {
	return workRectOf(c.currentScreenLocked())
}

// panelScreenLocked 解析面板的目标屏幕：优先用记下的 Name（副屏跨会话恢复）；
// 没有就按绝对坐标找最近的屏幕；再退到窗口当前屏幕（首次进入简约界面）。
func (c *windowController) panelScreenLocked() *application.Screen {
	var screens []*application.Screen
	if c.screens != nil {
		screens = c.screens()
	}
	if sc := screenByName(screens, c.panelScreen); sc != nil {
		return sc
	}
	if c.panelCX != 0 {
		if sc := screenNearestPoint(screens, c.panelCX, c.anchorY); sc != nil {
			return sc
		}
	}
	return c.currentScreenLocked()
}

// dockScreenLocked 解析吸边图标的目标屏幕：优先用记下的 Name；旧状态没有图标
// 屏幕名时与面板同屏；再退到窗口当前屏幕。
func (c *windowController) dockScreenLocked() *application.Screen {
	var screens []*application.Screen
	if c.screens != nil {
		screens = c.screens()
	}
	if sc := screenByName(screens, c.dockScreen); sc != nil {
		return sc
	}
	if c.panelCX != 0 {
		if sc := screenByName(screens, c.panelScreen); sc != nil {
			return sc
		}
		if sc := screenNearestPoint(screens, c.panelCX, c.anchorY); sc != nil {
			return sc
		}
	}
	return c.currentScreenLocked()
}

// dockWorkAreaLocked 返回吸边图标所在屏幕的工作区。
func (c *windowController) dockWorkAreaLocked() (uiRect, bool) {
	return workRectOf(c.dockScreenLocked())
}

// panelPlacementLocked 计算面板矩形和落地屏幕名。
func (c *windowController) panelPlacementLocked() (uiRect, string, bool) {
	sc := c.panelScreenLocked()
	work, ok := workRectOf(sc)
	if !ok {
		return uiRect{}, "", false
	}
	cy := c.anchorY
	if cy <= 0 {
		cy = work.centerY()
	}
	var r uiRect
	if c.panelCX != 0 {
		r = panelRect(work, miniWidth, miniHeight, c.panelCX, cy)
	} else {
		r = floatingPanelRect(work, miniWidth, miniHeight, cy, miniEdgeGap, c.edge)
	}
	name := ""
	if sc != nil {
		name = sc.Name
	}
	return r, name, true
}

// rememberPanelLocked 记录面板实际占用的中心点与屏幕名：x/y 都记，
// 展开、切回简约界面和重启都按它恢复。
func (c *windowController) rememberPanelLocked(r uiRect, screen string) {
	c.panelCX = r.centerX()
	c.anchorY = r.centerY()
	if screen != "" {
		c.panelScreen = screen
	}
}

func (c *windowController) applyStandardLocked(work uiRect) {
	c.win.SetFrameless(false)
	if c.style != nil {
		c.style.setMini(false)
	}
	c.win.SetResizable(true)
	c.win.SetMinSize(minWidth, minHeight)
	c.win.SetMaxSize(0, 0) // 0 = 不限制；必须在 SetSize 之前清掉简约界面的上限
	c.win.SetSize(c.stdW, c.stdH)
	c.win.SetAlwaysOnTop(false)
	r := centeredRect(work, c.stdW, c.stdH)
	c.win.SetPosition(r.X, r.Y)
}

func (c *windowController) applyMiniLocked() {
	c.win.SetFrameless(true)
	// 去掉标题栏样式位：否则 Windows 会把窗口最小宽度锁在约 131px，
	// 收起成 64x64 图标时会被撑宽并把图标挤出屏幕（用户看到"没有图标"）。
	if c.style != nil {
		c.style.setMini(true)
	}
	c.win.SetResizable(false)
	c.win.SetMinSize(miniWidth, miniHeight)
	c.win.SetMaxSize(miniWidth, miniHeight)
	c.win.SetSize(miniWidth, miniHeight)
	c.win.SetAlwaysOnTop(c.effectiveTopLocked())
	if r, screen, ok := c.panelPlacementLocked(); ok {
		c.win.SetPosition(r.X, r.Y)
		c.rememberPanelLocked(r, screen)
	}
}

func (c *windowController) collapseLocked(work, win uiRect) {
	c.panelCX = win.centerX()
	c.anchorY = win.centerY()
	if sc := c.currentScreenLocked(); sc != nil {
		c.panelScreen = sc.Name
		c.dockScreen = sc.Name
	}
	// 图标顶边对齐面板顶边（"图标在面板上沿"）；底部越界时 dockedRect 会夹回工作区。
	c.dockAnchorY = win.Y + dockSize/2
	c.applyDockGeometryLocked(work)
}

// applyDockGeometryLocked 把窗口收成 64x64 图标并按 dockAnchorY/edge 摆放。
// ApplyNativeStyle 在启动时也复用它：建窗时 Wails 仍带着标题栏样式位，Windows
// 可能把 64x64 撑开/挪位，导致"直接双击启动只露出一点图标"；去掉样式位后必须
// 重新套一遍几何（等价于切到标准界面再切回来的修复）。
func (c *windowController) applyDockGeometryLocked(work uiRect) {
	c.collapsed = true
	c.win.SetResizable(false)
	c.win.SetMinSize(dockSize, dockSize)
	c.win.SetMaxSize(dockSize, dockSize)
	c.win.SetSize(dockSize, dockSize)
	c.win.SetAlwaysOnTop(c.effectiveTopLocked())
	y := c.dockAnchorY
	if y <= 0 {
		if panel, _, ok := c.panelPlacementLocked(); ok {
			y = panel.Y + dockSize/2
		}
		if y <= 0 {
			y = work.centerY()
		}
	}
	r := dockedRect(work, dockSize, y, c.edge)
	c.dockAnchorY = r.centerY()
	c.win.SetPosition(r.X, r.Y)
}

func (c *windowController) expandLocked() {
	c.collapsed = false
	c.win.SetResizable(false)
	c.win.SetMinSize(miniWidth, miniHeight)
	c.win.SetMaxSize(miniWidth, miniHeight)
	c.win.SetSize(miniWidth, miniHeight)
	c.win.SetAlwaysOnTop(c.effectiveTopLocked())
	if r, screen, ok := c.panelPlacementLocked(); ok {
		c.win.SetPosition(r.X, r.Y)
		c.rememberPanelLocked(r, screen)
	}
}

// ApplyNativeStyle 在建窗完成后把状态文件里的模式同步到原生窗口样式，并重新
// 套一遍几何。
//
// 为什么必须重套几何：Wails 用 WS_OVERLAPPEDWINDOW 创建窗口，即 Frameless 也只是
// 随后 WM_NCCALCSIZE 削掉标题栏；Windows 在这期间会按标题栏窗口的最小尺寸/位置
// 规则处理，64x64 的吸边图标会被撑开或挤出屏幕（表现为"只露出一点点"）。切到
// 标准界面再切回来之所以正常，是因为那条路径在样式位去掉之后又 SetSize/SetPosition
// 了一次。这里在建窗回调里做同样的事。
func (c *windowController) ApplyNativeStyle() {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	if c.stopped || c.style == nil || c.win.NativeWindow() == 0 {
		return
	}
	if c.mode != UIModeMini {
		c.style.setMini(false)
		return
	}
	c.style.setMini(true)
	if c.collapsed {
		// 用图标自己那块屏的工作区修正；建窗时窗口可能已经被系统挪到别的屏上。
		work, ok := c.dockWorkAreaLocked()
		if !ok {
			work, ok = c.workAreaLocked()
		}
		if !ok {
			return
		}
		c.applyDockGeometryLocked(work)
	} else {
		c.applyMiniLocked()
	}
	c.guardLocked()
}

// ApplyNativeStyleWhenReady 等到原生窗口句柄就绪后再套样式/几何。
//
// Wails 把建窗放到 Run 之后的 goroutine 里，ApplicationStarted 回调可能先于
// 建窗执行（此时 NativeWindow()==0，直接 setMini 是空操作）。轮询一小段时间
// 等句柄出现；前端运行时就绪事件还会再调一次 ApplyNativeStyle 兜底。
func (c *windowController) ApplyNativeStyleWhenReady() {
	deadline := time.Now().Add(3 * time.Second)
	for {
		if c.win.NativeWindow() != 0 {
			c.ApplyNativeStyle()
			return
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// guardLocked 打一个"程序化调整中"的时间窗，避免位置回调把刚摆好的窗口当成用户拖动。
func (c *windowController) guardLocked() {
	c.guardUntil = time.Now().Add(c.guardWindow)
}

// ---------- 持久化与通知 ----------

func (c *windowController) persistLocked() {
	if c.stopped {
		return
	}
	c.dirty = true
	if c.saveTimer != nil {
		c.saveTimer.Stop()
	}
	c.saveTimer = time.AfterFunc(c.saveDelay, func() {
		c.opMu.Lock()
		defer c.opMu.Unlock()
		c.saveTimer = nil
		if c.stopped || !c.dirty {
			return
		}
		c.writeStateLocked()
	})
}

func (c *windowController) writeStateLocked() {
	c.dirty = false
	pinned := c.pinned
	windowStateWrite(c.path, windowState{
		Width:       c.stdW,
		Height:      c.stdH,
		Mode:        c.mode,
		Pinned:      &pinned,
		Edge:        c.edge,
		AnchorY:     c.anchorY,
		PanelCX:     c.panelCX,
		PanelScreen: c.panelScreen,
		DockScreen:  c.dockScreen,
		DockAnchorY: c.dockAnchorY,
		Collapsed:   c.collapsed,
	})
}

func (c *windowController) changedLocked() {
	if c.onChange == nil {
		return
	}
	c.onChange(c.stateLocked())
}
