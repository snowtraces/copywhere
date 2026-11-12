package wailsui

import (
	"context"
	"embed"
	"fmt"
	"io"
	"io/fs"
	"log"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"

	appcore "copywhere/internal/app"
	"copywhere/internal/config"
	"copywhere/internal/panel"
)

//go:embed all:frontend
var frontendAssets embed.FS

// 窗口默认尺寸与最小尺寸（逻辑像素）。
const (
	defaultWidth  = 1120
	defaultHeight = 760
	minWidth      = 920
	minHeight     = 600
)

// RunOptions 是 Wails 桌面前端的启动参数。
type RunOptions struct {
	Core    *appcore.App
	Cfg     *config.Config
	CfgPath string
	Bus     *panel.Bus

	// Smoke 非零时，窗口起来后等待该时长自动退出（构建/冒烟测试用）。
	Smoke time.Duration
	// LogWriter 非 nil 时 Wails 自身的警告/错误也写进该文件（与 wails.log 同源）。
	LogWriter io.Writer
}

// 二次启动 / IPC 唤回时先调用它：简约界面若已收起到屏幕边缘，先展开面板，
// 否则 SW_RESTORE 后用户只看到一个小图标，会以为窗口没打开。
var (
	revealHookMu sync.RWMutex
	revealHook   func()
)

func setRevealHook(fn func()) {
	revealHookMu.Lock()
	revealHook = fn
	revealHookMu.Unlock()
}

func callRevealHook() {
	revealHookMu.RLock()
	fn := revealHook
	revealHookMu.RUnlock()
	if fn != nil {
		fn()
	}
}

// Run 创建 Wails v3 应用、窗口与托盘，阻塞直到用户退出。
//
// 调用方（cmd/copywhere）负责：加载配置、抢单实例锁、启动核心服务与事件源，
// 以及在 Run 返回后 cancel 上下文并等待核心退出。
func Run(opts RunOptions) error {
	if opts.Core == nil || opts.Cfg == nil || opts.Bus == nil {
		return fmt.Errorf("wailsui: 缺少核心运行参数（core/cfg/bus 不能为空）")
	}
	assetsFS, err := fs.Sub(frontendAssets, "frontend")
	if err != nil {
		return fmt.Errorf("wailsui: 载入前端资源失败: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	svc := NewService(opts.Core, opts.Cfg, opts.CfgPath, opts.Bus)

	// showWindow 在窗口创建后赋值；第二实例唤起回调可能在窗口就绪前后触发。
	var showWindow func()

	winOpts := application.WindowsOptions{
		// 关闭窗口 = 收进托盘，进程继续同步。
		DisableQuitOnLastWindowClosed: true,
	}
	// 调试/自动化：CW_WAILS_DEBUG_PORT=9222 时打开 WebView2 远程调试端口，
	// 便于用 CDP 截图或排查前端问题（日常运行不开启）。
	if port := os.Getenv("CW_WAILS_DEBUG_PORT"); port != "" {
		winOpts.AdditionalBrowserArgs = []string{"--remote-debugging-port=" + port}
	}

	wailsApp := application.New(application.Options{
		Name:        "copywhere",
		Description: "局域网剪贴板 / 文件自动同步 + 鼠标键盘跨屏",
		Services: []application.Service{
			application.NewService(svc),
		},
		Assets: application.AssetOptions{
			Handler:        application.BundledAssetFileServer(assetsFS),
			DisableLogging: true,
		},
		Windows: winOpts,
		SingleInstance: &application.SingleInstanceOptions{
			UniqueID: "copywhere.desktop.wails",
			OnSecondInstanceLaunch: func(application.SecondInstanceData) {
				log.Printf("检测到第二次启动，激活已有窗口")
				if showWindow != nil {
					showWindow()
				}
			},
		},
		Logger:   wailsLogger(opts.LogWriter),
		LogLevel: slog.LevelWarn,
		ErrorHandler: func(err error) {
			log.Printf("Wails: %v", err)
		},
	})

	// 服务 → 前端的事件通道（日志/记录/进度/配对请求 + 周期快照）。
	svc.setEmitter(func(name string, data any) {
		wailsApp.Event.Emit(name, data)
	})
	go svc.StartBridge(ctx)

	statePath := windowStatePath(opts.CfgPath)
	st := readWindowState(statePath)
	screens := func() []*application.Screen { return wailsApp.Screen.GetAll() }
	boot := newWindowBootstrap(screens, st)

	var quitting atomic.Bool
	win := wailsApp.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:            "main",
		Title:           windowTitle,
		Width:           boot.width,
		Height:          boot.height,
		MinWidth:        boot.minWidth,
		MinHeight:       boot.minHeight,
		MaxWidth:        boot.maxWidth,
		MaxHeight:       boot.maxHeight,
		Frameless:       boot.frameless,
		DisableResize:   boot.disableResize,
		AlwaysOnTop:     boot.alwaysOnTop,
		InitialPosition: boot.position,
		X:               boot.x,
		Y:               boot.y,
		URL:             "/",
		EnableFileDrop:  true,
		DevToolsEnabled: os.Getenv("CW_WAILS_DEVTOOLS") == "1",
		// 透明背景：吸边收起后只露出应用图标本身（不需要深色方框）。
		// 标准界面与简约面板都用不透明 CSS 背景，观感不受影响。
		BackgroundType:             application.BackgroundTypeTransparent,
		BackgroundColour:           application.NewRGB(10, 14, 19),
		DefaultContextMenuDisabled: true,
		// 无边框模式不要 Aero 阴影/圆角：首次以简约/吸边态建窗时，这些装饰会在
		// 64x64 图标外面画出一圈灰色圆角框（切到标准界面再切回来就消失，因为那条
		// 路径走的是运行时 SetFrameless）。图标必须只剩图标本身。
		Windows: application.WindowsWindow{
			DisableFramelessWindowDecorations: true,
		},
	})

	uiWin := wailsWindow{win: win}
	ctl := newWindowController(uiWin, nativeStyler{win: uiWin}, statePath, st, screens)
	svc.setUI(ctl)
	setRevealHook(ctl.Reveal)
	defer setRevealHook(nil)

	showWindow = func() {
		ctl.Reveal() // 简约界面收起到边缘时，先展开面板再显示
		win.Show()
		win.Focus()
	}

	var quitOnce sync.Once
	quit := func() {
		quitOnce.Do(func() {
			quitting.Store(true)
			ctl.Stop()
			cancel()
			wailsApp.Quit()
		})
	}

	// 关闭 = 收进托盘；从托盘退出时（quitting）放行真正关闭。
	win.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		if quitting.Load() {
			return
		}
		log.Printf("窗口关闭：收进托盘，同步继续在后台运行")
		ctl.Flush()
		win.Hide()
		e.Cancel()
	})
	win.OnWindowEvent(events.Common.WindowDidMove, func(*application.WindowEvent) { ctl.onMoved() })
	win.OnWindowEvent(events.Common.WindowDidResize, func(*application.WindowEvent) { ctl.onResized() })

	// 文件拖入窗口：拿到的是真实路径，直接投送，无需先上传临时目录。
	win.OnWindowEvent(events.Common.WindowFilesDropped, func(e *application.WindowEvent) {
		files := e.Context().DroppedFiles()
		if len(files) == 0 {
			return
		}
		go func() {
			if err := svc.SendFiles(files); err != nil {
				svc.toast(err.Error(), true)
				return
			}
			svc.toast(fmt.Sprintf("已投送 %d 项内容到在线节点", len(files)), false)
		}()
	})

	// 前端运行时就绪 = 原生窗口一定已创建：再套一次原生样式与几何兜底。
	// （ApplicationStarted 可能早于 Wails 的建窗 goroutine，见 ApplyNativeStyleWhenReady。）
	win.OnWindowEvent(events.Common.WindowRuntimeReady, func(*application.WindowEvent) {
		ctl.ApplyNativeStyle()
	})

	wailsApp.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
		// 建窗后原生句柄才可用：把"简约界面 = 无标题栏样式位"补上并重新套几何，
		// 否则收起成 64x64 图标时会被系统最小宽度撑开/挤出屏幕。
		// 这里异步等句柄就绪：Wails 的建窗在 Run 之后的 goroutine 里。
		go ctl.ApplyNativeStyleWhenReady()
		ui := ctl.State()
		log.Printf("Wails 桌面窗口已启动：%dx%d（%s 界面；关闭窗口将收进托盘继续同步）",
			ui.Width, ui.Height, ui.Mode)
		svc.EmitSnapshot()
		// 前端可能错过启动瞬间的 cw:ui：这里再推一次当前模式。
		wailsApp.Event.Emit("cw:ui", ui)
		if os.Getenv("CW_WAILS_DEVTOOLS") == "1" {
			win.OpenDevTools()
		}
	})

	wailsApp.OnShutdown(func() {
		ctl.Stop()
		cancel()
	})

	buildTray(wailsApp, svc, opts.Core, ctl, showWindow, quit)

	if opts.Smoke > 0 {
		go func() {
			time.Sleep(opts.Smoke)
			log.Printf("wailsui: 冒烟测试到达时限（%s），自动退出", opts.Smoke)
			quit()
		}()
	}

	if err := wailsApp.Run(); err != nil {
		return fmt.Errorf("Wails 应用异常退出: %w", err)
	}
	return nil
}

// windowBootstrap 把窗口状态文件翻译成 Wails 建窗参数。
// 简约界面要在一开始就按窄面板（或上次留下的 64x64 吸边图标）建窗，
// 避免"先开大窗口再缩"的闪一下。
type windowBootstrap struct {
	width, height            int
	minWidth, minHeight      int
	maxWidth, maxHeight      int
	frameless, disableResize bool
	alwaysOnTop              bool
	position                 application.WindowStartPosition
	x, y                     int
}

func newWindowBootstrap(screens func() []*application.Screen, st windowState) windowBootstrap {
	opts := windowBootstrap{
		width:       st.Width,
		height:      st.Height,
		minWidth:    minWidth,
		minHeight:   minHeight,
		position:    application.WindowCentered,
		frameless:   false,
		alwaysOnTop: false,
	}
	if normalizeUIMode(st.Mode) != UIModeMini {
		return opts
	}
	pinned := st.Pinned != nil && *st.Pinned
	opts.frameless = true
	opts.disableResize = true
	if st.Collapsed {
		// 上次退出时停在边缘图标：直接按 64x64 透明小窗建出来，落在同一位置。
		opts.width, opts.height = dockSize, dockSize
		opts.minWidth, opts.minHeight = dockSize, dockSize
		opts.maxWidth, opts.maxHeight = dockSize, dockSize
		// 与 effectiveTopLocked 一致：收起成图标时必须置顶。
		opts.alwaysOnTop = true
	} else {
		opts.width, opts.height = miniWidth, miniHeight
		opts.minWidth, opts.minHeight = miniWidth, miniHeight
		opts.maxWidth, opts.maxHeight = miniWidth, miniHeight
		opts.alwaysOnTop = pinned
	}
	if screens == nil {
		return opts
	}
	all := screens()

	// 面板：优先按记下的屏幕名（副屏跨会话稳定）恢复；旧状态没有屏幕名时
	// 用绝对坐标反查最近的屏幕，再退到主屏。
	panelScreen := screenByName(all, st.PanelScreen)
	if panelScreen == nil && st.PanelCX != 0 {
		panelScreen = screenNearestPoint(all, st.PanelCX, st.AnchorY)
	}
	if panelScreen == nil {
		panelScreen = screensPrimary(all)
	}
	panelWork, ok := workRectOf(panelScreen)
	if !ok {
		return opts
	}
	panelCY := st.AnchorY
	if panelCY <= 0 {
		panelCY = panelWork.centerY()
	}
	var panel uiRect
	if st.PanelCX != 0 {
		panel = panelRect(panelWork, miniWidth, miniHeight, st.PanelCX, panelCY)
	} else {
		panel = floatingPanelRect(panelWork, miniWidth, miniHeight, panelCY, miniEdgeGap, st.Edge)
	}

	r := panel
	if st.Collapsed {
		// 图标：按记下的图标屏幕恢复；旧状态没有就与面板同屏。
		dockScreen := screenByName(all, st.DockScreen)
		if dockScreen == nil {
			dockScreen = panelScreen
		}
		dockWork, ok := workRectOf(dockScreen)
		if !ok {
			dockWork = panelWork
		}
		dockAnchor := st.DockAnchorY
		if dockAnchor <= 0 {
			// 没有图标锚点（旧状态/手改文件）：按"图标顶边对齐面板顶边"推算。
			dockAnchor = panel.Y + dockSize/2
		}
		r = dockedRect(dockWork, dockSize, dockAnchor, st.Edge)
	}
	opts.position = application.WindowXY
	opts.x, opts.y = r.X, r.Y
	return opts
}

// buildTray 创建系统托盘与菜单。
//
// 菜单只保留运行期需要的基础项；界面模式由窗口里的按钮/Ctrl+M 切换，并写在
// wails-window.json 里，下次启动沿用（默认记住上一次是简约还是标准界面）。
func buildTray(app *application.App, svc *Service, core *appcore.App, ctl *windowController, showWindow, quit func()) {
	tray := app.SystemTray.New()
	tray.SetIcon(panel.AppIcon())
	tray.SetTooltip("copywhere — 双击打开窗口，右键打开菜单")

	menu := app.NewMenu()
	menu.Add("打开 copywhere 窗口").OnClick(func(*application.Context) { showWindow() })
	menu.AddSeparator()

	syncItem := menu.AddCheckbox("自动同步", !core.IsPaused())
	syncItem.OnClick(func(ctx *application.Context) {
		on := ctx.ClickedMenuItem().Checked()
		core.SetPaused(!on)
		svc.SetPausedFromTray(!on)
	})
	svc.setPauseHook(func(paused bool) {
		syncItem.SetChecked(!paused)
	})

	menu.Add("打开接收目录").OnClick(func(*application.Context) {
		if err := svc.OpenReceiveDir(); err != nil {
			log.Printf("打开接收目录失败: %v", err)
		}
	})
	menu.AddSeparator()
	menu.Add("退出").OnClick(func(*application.Context) { quit() })

	ctl.setOnChange(func(st UiState) {
		app.Event.Emit("cw:ui", st)
	})

	tray.SetMenu(menu)

	// 托盘点击：左键双击直接打开窗口；单击仍打开菜单，但要等一个双击窗口再弹，
	// 否则第一次单击就会进入菜单的模态循环，双击事件永远到不了托盘窗口。
	// 右键直接弹菜单（NOTIFYICON_VERSION_4 下是 WM_CONTEXTMENU）。
	var (
		clickMu      sync.Mutex
		menuTimer    *time.Timer
		suppressTill time.Time
	)
	showFromTray := func() {
		clickMu.Lock()
		if menuTimer != nil {
			menuTimer.Stop()
			menuTimer = nil
		}
		// Windows 在双击后还会补发一次 WM_LBUTTONUP；抑制它，避免刚打开窗口
		// 又弹出菜单。
		suppressTill = time.Now().Add(600 * time.Millisecond)
		clickMu.Unlock()
		showWindow()
	}
	tray.OnClick(func() {
		clickMu.Lock()
		if time.Now().Before(suppressTill) {
			clickMu.Unlock()
			return
		}
		if menuTimer != nil {
			menuTimer.Stop()
		}
		menuTimer = time.AfterFunc(trayMenuDelay(), func() {
			clickMu.Lock()
			menuTimer = nil
			clickMu.Unlock()
			tray.OpenMenu()
		})
		clickMu.Unlock()
	})
	tray.OnDoubleClick(showFromTray)
	tray.OnRightClick(func() { tray.OpenMenu() })
}

// ---------- Wails 日志 ----------

// wailsLogger 把 Wails 框架自身的日志并入 copywhere 的日志文件。
// 未提供文件时返回 nil，使用 Wails 默认日志。
func wailsLogger(w io.Writer) *slog.Logger {
	if w == nil {
		return nil
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelWarn}))
}
