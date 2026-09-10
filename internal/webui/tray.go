package webui

import (
	_ "embed"

	"fyne.io/systray"
)

//go:embed icon.ico
var iconICO []byte

//go:embed favicon.svg
var faviconSVG []byte

//go:embed favicon.png
var faviconPNG []byte

//go:embed icons/menu_open.ico
var iconOpen []byte

//go:embed icons/menu_pause.ico
var iconPause []byte

//go:embed icons/menu_resume.ico
var iconResume []byte

//go:embed icons/menu_folder.ico
var iconFolder []byte

//go:embed icons/menu_quit.ico
var iconQuit []byte

//go:embed icons/icon_paused.ico
var iconPaused []byte

// TrayOptions 是托盘对宿主的回调集合。
type TrayOptions struct {
	OpenPanel      func() // 打开浏览器面板
	OpenReceiveDir func() // 打开接收目录
	IsPaused       func() bool
	SetPaused      func(bool)
	Quit           func() // 请求宿主退出（托盘自身随后 Quit）
}

// RunTray 阻塞运行系统托盘图标，直到退出菜单项或宿主调用 QuitTray。
func RunTray(opts TrayOptions) {
	systray.Run(func() {
		systray.SetTitle("copywhere")
		mOpen := systray.AddMenuItem("打开面板", "在浏览器中打开控制面板")
		mOpen.SetIcon(iconOpen)

		systray.AddSeparator()
		mPause := systray.AddMenuItem("暂停自动同步", "暂停/恢复剪贴板自动同步（手动发送不受影响）")
		if opts.IsPaused != nil && opts.IsPaused() {
			systray.SetIcon(iconPaused)
			systray.SetTooltip("copywhere — 自动同步已暂停")
			mPause.SetTitle("恢复自动同步")
			mPause.SetIcon(iconResume)
		} else {
			systray.SetIcon(iconICO)
			systray.SetTooltip("copywhere — 局域网剪贴板/文件自动同步")
			mPause.SetIcon(iconPause)
		}

		mDir := systray.AddMenuItem("打开接收目录", "在资源管理器中打开接收目录")
		mDir.SetIcon(iconFolder)

		systray.AddSeparator()
		mQuit := systray.AddMenuItem("退出", "退出 copywhere")
		mQuit.SetIcon(iconQuit)

		for {
			select {
			case <-mOpen.ClickedCh:
				opts.OpenPanel()
			case <-mPause.ClickedCh:
				next := !opts.IsPaused()
				opts.SetPaused(next)
				if next {
					systray.SetIcon(iconPaused)
					systray.SetTooltip("copywhere — 自动同步已暂停")
					mPause.SetTitle("恢复自动同步")
					mPause.SetIcon(iconResume)
				} else {
					systray.SetIcon(iconICO)
					systray.SetTooltip("copywhere — 局域网剪贴板/文件自动同步")
					mPause.SetTitle("暂停自动同步")
					mPause.SetIcon(iconPause)
				}
			case <-mDir.ClickedCh:
				opts.OpenReceiveDir()
			case <-mQuit.ClickedCh:
				opts.Quit()
				systray.Quit()
				return
			}
		}
	}, nil)
}

// QuitTray 请求托盘退出（宿主主动关闭时调用）。
func QuitTray() { systray.Quit() }
