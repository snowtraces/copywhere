//go:build windows

// 剪贴板变更事件监听（借鉴 PowerToys MouseWithoutBorders 的事件驱动检测思路）。
//
// 原理：创建一条永久锁定在单一 OS 线程上的监听 goroutine，在其上注册窗口类、
// 创建 message-only 窗口（父句柄 HWND_MESSAGE），再调用
// AddClipboardFormatListener 把该窗口挂进系统剪贴板监听链。此后每次剪贴板
// 内容变化，系统都会向窗口投递 WM_CLIPBOARDUPDATE——检测结果即时到达，
// 无需等待下一个轮询周期。
//
// 与 clipboardThread（读写线程）刻意分离：那条线程的生命线是
// OpenClipboard/CloseClipboard 严格同线程配对，绝不能在消息泵里做
// 可能长期阻塞的读写；本文件只收通知、不碰剪贴板内容，回调里仅向
// 带缓冲通道做一次非阻塞投递，消息泵永不阻塞。
//
// 轮询（clip.Seq 比较序号）仍然保留为兜底通道：监听安装失败
// （极端环境、窗口创建失败）或事件意外丢失时，检测退化为原 400ms 轮询，
// 功能不受影响。

package clip

import (
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"copywhere/internal/diag"
)

const (
	wmDestroy         = 0x0002
	wmDrawClipboard   = 0x0308 // 旧式查看链（SetClipboardViewer）的变更通知
	wmChangeCBChain   = 0x030D // 查看链上有窗口摘除，需维护 nextViewer
	wmClipboardUpdate = 0x031D // AddClipboardFormatListener 的变更通知

	hwndMessage = ^uintptr(2) // (HWND)HWND_MESSAGE：message-only 窗口父句柄
)

var (
	procAddClipboardFormatListener    = user32.NewProc("AddClipboardFormatListener")
	procRemoveClipboardFormatListener = user32.NewProc("RemoveClipboardFormatListener")
	procSetClipboardViewer            = user32.NewProc("SetClipboardViewer")
	procChangeClipboardChain          = user32.NewProc("ChangeClipboardChain")
	procRegisterClassExW              = user32.NewProc("RegisterClassExW")
	procCreateWindowExW               = user32.NewProc("CreateWindowExW")
	procGetMessageW                   = user32.NewProc("GetMessageW")
	procTranslateMessage              = user32.NewProc("TranslateMessage")
	procDispatchMessageW              = user32.NewProc("DispatchMessageW")
	procDefWindowProcW                = user32.NewProc("DefWindowProcW")
	procSendMessageW                  = user32.NewProc("SendMessageW")
)

// clipWndClassEx 对应 Win32 WNDCLASSEXW；与 input 包中的同名结构布局一致
// （两包独立编译，不复用类型定义）。
type clipWndClassEx struct {
	cbSize, style                            uint32
	wndProc                                  uintptr
	cbClsExtra, cbWndExtra                   int32
	hInstance, hIcon, hCursor, hbrBackground uintptr
	lpszMenuName                             *uint16
	lpszClassName                            *uint16
	hIconSm                                  uintptr
}

// clipMsg 对应 Win32 MSG。
type clipMsg struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      [2]int32
}

var (
	watchOnce        sync.Once
	watchOK          atomic.Bool // 监听是否安装成功（StartWatch 的返回值）
	watchFn          func()      // 变更回调；在监听 goroutine 启动前写入，之后只读
	nextViewer       uintptr     // 查看链下一环（仅监听线程读写）
	usingViewerChain atomic.Bool // true = 走 SetClipboardViewer 旧链兜底（跨线程读）
)

// StartWatch 安装剪贴板变更事件监听；进程内只安装一次，重复调用返回
// 首次安装结果。onChange 在监听线程上被调用，必须快速返回、绝不阻塞
// （消息泵阻塞会延迟系统对全部监听者的通知）。返回 false 表示安装失败，
// 调用方应继续依赖轮询兜底。
//
// 生命周期契约：回调永久绑定首次调用者——同一进程内第二个调用者的回调
// 永远不会被触发（当前仅 monitor.Run 使用且两条启动路径互斥；若未来需要
// 多实例，应改为回调注册表而非在此打补丁）。
func StartWatch(onChange func()) bool {
	if onChange == nil {
		return false
	}
	watchOnce.Do(func() {
		watchFn = onChange
		watchOK.Store(startWatchThread())
	})
	return watchOK.Load()
}

// WatchMode 返回当前监听方式："listener"（现代 API）、"viewer"（经典链兜底）、
// "none"（未安装成功，仅轮询）。供诊断面板展示。
func WatchMode() string {
	switch {
	case !watchOK.Load():
		return "none"
	case usingViewerChain.Load():
		return "viewer"
	default:
		return "listener"
	}
}

// startWatchThread 启动监听 goroutine 并同步等待安装结果。
// 超时兜底：监听线程若被调度卡住（理论上不会），返回 false 让调用方
// 退化为纯轮询，而不是把整个监控启动拖死。
func startWatchThread() bool {
	ready := make(chan bool, 1)
	go watchThread(ready)
	select {
	case ok := <-ready:
		return ok
	case <-time.After(3 * time.Second):
		return false
	}
}

// watchThread 常驻运行：建窗口、挂监听、泵消息。全程锁定单一 OS 线程——
// AddClipboardFormatListener 与窗口消息回路都绑定线程消息队列，且
// windows.NewCallback 注册的 wndProc 一生只创建一次，配额无忧。
func watchThread(ready chan<- bool) {
	runtime.LockOSThread()

	clsName := windows.StringToUTF16Ptr("copywhereClipWatchWnd")
	procRegisterClassExW.Call(uintptr(unsafe.Pointer(&clipWndClassEx{
		cbSize:        uint32(unsafe.Sizeof(clipWndClassEx{})),
		wndProc:       windows.NewCallback(clipWndProc),
		lpszClassName: clsName,
	})))
	hwnd, _, _ := procCreateWindowExW.Call(0,
		uintptr(unsafe.Pointer(clsName)),
		uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(""))),
		0, 0, 0, 0, 0, hwndMessage, 0, 0, 0)
	if hwnd == 0 {
		ready <- false
		return
	}

	r, _, _ := procAddClipboardFormatListener.Call(hwnd)
	mode := "listener"
	if r == 0 {
		// Vista 监听 API 失败（理论上仅极端环境）：退回经典查看链。
		nv, _, _ := procSetClipboardViewer.Call(hwnd)
		if nv == 0 {
			ready <- false
			return
		}
		nextViewer = nv
		usingViewerChain.Store(true)
		mode = "viewer"
	}
	// 用局部结果记会话：此刻 watchOK 尚未 Store，调 WatchMode() 会恒得 "none"。
	diag.SetSession("clipboard_watch", mode)
	ready <- true

	var m clipMsg
	for {
		ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if ret == 0 || ret == ^uintptr(0) {
			return // WM_QUIT 或出错：线程退出（进程退出路径，无需解锁线程）
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

// clipWndProc 是监听窗口的消息过程。只在监听线程上被系统调用。
func clipWndProc(hwnd, uMsg, wParam, lParam uintptr) uintptr {
	defer func() { _ = recover() }() // 监听线程绝不能 panic（会带崩整个进程）
	switch uMsg {
	case wmClipboardUpdate, wmDrawClipboard:
		if uMsg == wmDrawClipboard && usingViewerChain.Load() && nextViewer != 0 {
			// 旧链语义：把 WM_DRAWCLIPBOARD 原样转发给下一环（wParam=本窗口，
			// 让下一环认识新链头），否则链上我方之后的所有应用都收不到通知。
			procSendMessageW.Call(nextViewer, wmDrawClipboard, hwnd, 0)
		}
		if watchFn != nil {
			watchFn()
		}
		return 0
	case wmChangeCBChain:
		if usingViewerChain.Load() {
			if wParam == nextViewer {
				nextViewer = lParam // 下一环摘除，指向再下一环
			} else if nextViewer != 0 {
				procSendMessageW.Call(nextViewer, wmChangeCBChain, wParam, lParam)
			}
		}
		return 0
	case wmDestroy:
		if usingViewerChain.Load() {
			procChangeClipboardChain.Call(hwnd, nextViewer)
		} else {
			procRemoveClipboardFormatListener.Call(hwnd)
		}
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, uMsg, wParam, lParam)
	return r
}
