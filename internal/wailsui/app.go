package wailsui

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"log/slog"
	"os"
	"path/filepath"
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

	sizePath := windowStatePath(opts.CfgPath)
	st := readWindowState(sizePath)

	var quitting atomic.Bool
	win := wailsApp.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:                       "main",
		Title:                      windowTitle,
		Width:                      st.Width,
		Height:                     st.Height,
		MinWidth:                   minWidth,
		MinHeight:                  minHeight,
		URL:                        "/",
		InitialPosition:            application.WindowCentered,
		EnableFileDrop:             true,
		DevToolsEnabled:            os.Getenv("CW_WAILS_DEVTOOLS") == "1",
		BackgroundType:             application.BackgroundTypeSolid,
		BackgroundColour:           application.NewRGB(10, 14, 19),
		DefaultContextMenuDisabled: true,
	})

	showWindow = func() {
		win.Show()
		win.Focus()
	}

	// 尺寸记忆：拖动/缩放时去抖落盘，退出时再存一次。
	var saveMu sync.Mutex
	var saveTimer *time.Timer
	saveWindowSize := func() {
		w, h := win.Size()
		if w < minWidth || h < minHeight {
			return // 最小化/隐藏时尺寸无意义，别写坏配置
		}
		windowStateWrite(sizePath, windowState{Width: w, Height: h})
	}
	saveDebounced := func() {
		saveMu.Lock()
		defer saveMu.Unlock()
		if saveTimer != nil {
			saveTimer.Stop()
		}
		saveTimer = time.AfterFunc(400*time.Millisecond, saveWindowSize)
	}

	var quitOnce sync.Once
	quit := func() {
		quitOnce.Do(func() {
			quitting.Store(true)
			saveWindowSize()
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
		saveWindowSize()
		win.Hide()
		e.Cancel()
	})
	win.OnWindowEvent(events.Common.WindowDidMove, func(*application.WindowEvent) { saveDebounced() })
	win.OnWindowEvent(events.Common.WindowDidResize, func(*application.WindowEvent) { saveDebounced() })

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

	wailsApp.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
		log.Printf("Wails 桌面窗口已启动：%dx%d（关闭窗口将收进托盘继续同步）", win.Width(), win.Height())
		svc.EmitSnapshot()
		if os.Getenv("CW_WAILS_DEVTOOLS") == "1" {
			win.OpenDevTools()
		}
	})

	wailsApp.OnShutdown(func() {
		saveWindowSize()
		cancel()
	})

	buildTray(wailsApp, svc, opts.Core, showWindow, quit)

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

// buildTray 创建系统托盘与菜单。
func buildTray(app *application.App, svc *Service, core *appcore.App, showWindow, quit func()) {
	tray := app.SystemTray.New()
	tray.SetIcon(panel.AppIcon())
	tray.SetTooltip("copywhere — 局域网剪贴板 / 文件自动同步")

	menu := app.NewMenu()
	menu.Add("打开 copywhere 窗口").OnClick(func(*application.Context) { showWindow() })

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

	tray.SetMenu(menu)
	tray.OnClick(func() { tray.OpenMenu() })
}

// ---------- 窗口尺寸记忆 ----------

type windowState struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

// windowStatePath 返回窗口尺寸状态文件（与配置文件同目录）。
func windowStatePath(cfgPath string) string {
	dir := filepath.Dir(cfgPath)
	if cfgPath == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".copywhere")
	}
	return filepath.Join(dir, "wails-window.json")
}

func readWindowState(path string) windowState {
	var st windowState
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &st)
	}
	if st.Width < minWidth || st.Width > 6000 {
		st.Width = defaultWidth
	}
	if st.Height < minHeight || st.Height > 4000 {
		st.Height = defaultHeight
	}
	return st
}

// windowStateWrite 记录窗口尺寸：下次启动恢复（位置仍居中，避免显示器布局
// 变化后窗口跑到屏幕外）。
func windowStateWrite(path string, st windowState) {
	data, err := json.Marshal(st)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		log.Printf("记录窗口尺寸失败: %v", err)
	}
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
