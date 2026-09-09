//go:build windows

// Package input 封装 Windows 全局输入捕获（低级钩子 + Raw Input）与 SendInput 注入，
// 供 KVM（鼠标键盘跨屏）使用。
//
// 事件回调在系统钩子线程上执行，必须快速返回、不得阻塞——由 kvm 层保证。
package input

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	whMouseLL    = 14
	whKeyboardLL = 13

	wmInput       = 0x00FF
	wmMouseWheel  = 0x020A
	wmMouseHWheel = 0x020E

	wmLButtonDown = 0x0201
	wmLButtonUp   = 0x0202
	wmRButtonDown = 0x0204
	wmRButtonUp   = 0x0205
	wmMButtonDown = 0x0207
	wmMButtonUp   = 0x0208
	wmXButtonDown = 0x020B
	wmXButtonUp   = 0x020C

	llkhfExtended = 0x01
	llkhfInjected = 0x10
	llkhfUp       = 0x80

	llmhfInjected = 0x01

	smXVirtualScreen  = 76
	smYVirtualScreen  = 77
	smCXVirtualScreen = 78
	smCYVirtualScreen = 79

	ridevInputSink = 0x00000100
	ridInput       = 0x10000003

	inputMouse    = 0
	inputKeyboard = 1

	mouseEventfMove        = 0x0001
	mouseEventfLeftDown    = 0x0002
	mouseEventfLeftUp      = 0x0004
	mouseEventfRightDown   = 0x0008
	mouseEventfRightUp     = 0x0010
	mouseEventfMiddleDown  = 0x0020
	mouseEventfMiddleUp    = 0x0040
	mouseEventfXDown       = 0x0080
	mouseEventfXUp         = 0x0100
	mouseEventfWheel       = 0x0800
	mouseEventfHWheel      = 0x1000
	mouseEventfVirtualDesk = 0x4000
	mouseEventfAbsolute    = 0x8000

	keyeventfExtendedKey = 0x0001
	keyeventfKeyUp       = 0x0002

	hwndMessage = ^uintptr(2) // (HWND)-3
)

var (
	user32 = windows.NewLazySystemDLL("user32.dll")

	procSetWindowsHookExW       = user32.NewProc("SetWindowsHookExW")
	procUnhookWindowsHookEx     = user32.NewProc("UnhookWindowsHookEx")
	procCallNextHookEx          = user32.NewProc("CallNextHookEx")
	procGetMessageW             = user32.NewProc("GetMessageW")
	procTranslateMessage        = user32.NewProc("TranslateMessage")
	procDispatchMessageW        = user32.NewProc("DispatchMessageW")
	procDefWindowProcW          = user32.NewProc("DefWindowProcW")
	procRegisterClassExW        = user32.NewProc("RegisterClassExW")
	procCreateWindowExW         = user32.NewProc("CreateWindowExW")
	procRegisterRawInputDevices = user32.NewProc("RegisterRawInputDevices")
	procGetRawInputData         = user32.NewProc("GetRawInputData")
	procSendInput               = user32.NewProc("SendInput")
	procGetSystemMetrics        = user32.NewProc("GetSystemMetrics")
	procSetCursorPos            = user32.NewProc("SetCursorPos")
	procGetCursorPos            = user32.NewProc("GetCursorPos")
	procMapVirtualKeyW          = user32.NewProc("MapVirtualKeyW")
	procSetProcessDPIAware      = user32.NewProc("SetProcessDPIAware")
)

// Callbacks 是输入事件回调集合，全部在钩子线程上调用，必须非阻塞。
type Callbacks struct {
	// OnMouseMove 原始相对位移（Raw Input，未经系统加速/裁剪）。
	OnMouseMove func(dx, dy int)
	// OnMouseButton 按键事件。button: 1=左 2=右 3=中 4/5=侧键。
	OnMouseButton func(down bool, button int, x, y int)
	// OnWheel 滚轮。delta 常见为 ±120 的倍数。
	OnWheel func(delta int32, horizontal bool)
	// OnKey 键盘事件。
	OnKey func(vk, scan uint32, down, ext bool)
}

var (
	cbs      Callbacks
	suppress atomic.Bool // 为 true 时吞掉本地鼠标/键盘事件（主控机控制远端期间）
	started  atomic.Bool
)

// SetSuppress 控制是否吞掉本地输入事件。
func SetSuppress(b bool) { suppress.Store(b) }

// Start 安装全局输入钩子与 Raw Input 监听（进程级一次）。
func Start(cb Callbacks) error {
	if !started.CompareAndSwap(false, true) {
		return nil
	}
	cbs = cb
	procSetProcessDPIAware.Call() // 光标坐标与屏幕度量使用物理像素
	go hookThread()
	return nil
}

// DefaultInjector 是基于 SendInput 的注入器。
type DefaultInjector struct{}

func (DefaultInjector) MoveAbs(x, y int)                    { InjectMoveAbs(x, y) }
func (DefaultInjector) Button(down bool, button int)        { InjectButton(down, button) }
func (DefaultInjector) Wheel(delta int32, horizontal bool)  { InjectWheel(delta, horizontal) }
func (DefaultInjector) Key(vk, scan uint32, down, ext bool) { InjectKey(vk, scan, down, ext) }
func (DefaultInjector) ScreenBounds() (int, int, int, int)  { return VirtualScreen() }
func (DefaultInjector) CursorPos() (int, int)               { return CursorPos() }

// VirtualScreen 返回虚拟桌面 (x, y, w, h)。
func VirtualScreen() (x, y, w, h int) {
	x, y, w, h = smInt(smXVirtualScreen), smInt(smYVirtualScreen), smInt(smCXVirtualScreen), smInt(smCYVirtualScreen)
	if w <= 0 || h <= 0 {
		x, y, w, h = 0, 0, smInt(0), smInt(1)
	}
	return
}

func CursorPos() (int, int) {
	var pt point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	return int(pt.x), int(pt.y)
}

func SetCursorPos(x, y int) { procSetCursorPos.Call(uintptr(x), uintptr(y)) }

// InjectMoveAbs 在虚拟桌面绝对坐标处放置光标。
func InjectMoveAbs(x, y int) {
	vx, vy, vw, vh := VirtualScreen()
	if vw <= 1 || vh <= 1 {
		return
	}
	nx := (x - vx) * 65535 / (vw - 1)
	ny := (y - vy) * 65535 / (vh - 1)
	sendMouse(mouseEventfMove|mouseEventfAbsolute|mouseEventfVirtualDesk, int32(nx), int32(ny), 0)
}

// InjectButton 注入鼠标按键。button: 1=左 2=右 3=中 4/5=侧键。
func InjectButton(down bool, button int) {
	var f uint32
	var data int32
	switch button {
	case 1:
		if down {
			f = mouseEventfLeftDown
		} else {
			f = mouseEventfLeftUp
		}
	case 2:
		if down {
			f = mouseEventfRightDown
		} else {
			f = mouseEventfRightUp
		}
	case 3:
		if down {
			f = mouseEventfMiddleDown
		} else {
			f = mouseEventfMiddleUp
		}
	case 4, 5:
		if down {
			f = mouseEventfXDown
		} else {
			f = mouseEventfXUp
		}
		data = int32(button - 4 + 1)
	default:
		return
	}
	sendMouse(f, 0, 0, data)
}

// InjectWheel 注入滚轮。
func InjectWheel(delta int32, horizontal bool) {
	f := uint32(mouseEventfWheel)
	if horizontal {
		f = mouseEventfHWheel
	}
	sendMouse(f, 0, 0, delta)
}

// InjectKey 注入键盘事件。
func InjectKey(vk, scan uint32, down, ext bool) {
	if scan == 0 {
		r, _, _ := procMapVirtualKeyW.Call(uintptr(vk), 0) // MAPVK_VK_TO_VSC
		scan = uint32(r)
	}
	var f uint32
	if ext {
		f |= keyeventfExtendedKey
	}
	if !down {
		f |= keyeventfKeyUp
	}
	sendKey(vk, uint16(scan), f)
}

// ---------- 内部实现 ----------

type point struct{ x, y int32 }

type msllhookstruct struct {
	x, y      int32
	mouseData uint32
	flags     uint32
	time      uint32
	extra     uintptr
}

type kbdllhookstruct struct {
	vkCode   uint32
	scanCode uint32
	flags    uint32
	time     uint32
	extra    uintptr
}

type msg struct {
	hwnd, message  uintptr
	wParam, lParam uintptr
	time           uint32
	pt             point
}

type wndclassex struct {
	cbSize, style                            uint32
	wndProc                                  uintptr
	cbClsExtra, cbWndExtra                   int32
	hInstance, hIcon, hCursor, hbrBackground uintptr
	lpszMenuName                             *uint16
	lpszClassName                            *uint16
	hIconSm                                  uintptr
}

type rawinputdevice struct {
	usUsagePage, usUsage uint16
	dwFlags              uint32
	hwndTarget           uintptr
}

type rawinputHeader struct {
	dwType, dwSize  uint32
	hDevice, wParam uintptr
}

type rawmouse struct {
	usFlags        uint16
	_              uint16
	usButtonFlags  uint16
	usButtonData   uint16
	ulRawButtons   uint32
	lLastX, lLastY int32
	ulExtra        uint32
}

type mouseInput struct {
	dx, dy    int32
	mouseData uint32
	dwFlags   uint32
	time      uint32
	extra     uintptr
}

type keybdInput struct {
	wVk, wScan uint16
	dwFlags    uint32
	time       uint32
	extra      uintptr
}

type inputStruct struct {
	typ uint32
	_   [4]byte
	u   [32]byte
}

func smInt(n int32) int {
	r, _, _ := procGetSystemMetrics.Call(uintptr(n))
	return int(r)
}

var classRegistered sync.Once

func hookThread() {
	runtime.LockOSThread()

	procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wndclassex{
		cbSize:        uint32(unsafe.Sizeof(wndclassex{})),
		wndProc:       windows.NewCallback(wndProc),
		lpszClassName: windows.StringToUTF16Ptr("copywhereInputWnd"),
	})))
	hwnd, _, _ := procCreateWindowExW.Call(0,
		uintptr(unsafe.Pointer(windows.StringToUTF16Ptr("copywhereInputWnd"))),
		uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(""))),
		0, 0, 0, 0, 0, hwndMessage, 0, 0, 0)
	if hwnd == 0 {
		fmt.Println("copywhere/input: 创建输入窗口失败")
		return
	}

	// 注册 Raw Input：即使不在前台也接收鼠标原始输入
	dev := rawinputdevice{usUsagePage: 1, usUsage: 2, dwFlags: ridevInputSink, hwndTarget: hwnd}
	procRegisterRawInputDevices.Call(uintptr(unsafe.Pointer(&dev)), 1, unsafe.Sizeof(dev))

	hMouse, _, _ := procSetWindowsHookExW.Call(whMouseLL, windows.NewCallback(mouseHookProc), 0, 0)
	hKey, _, _ := procSetWindowsHookExW.Call(whKeyboardLL, windows.NewCallback(keyHookProc), 0, 0)
	defer func() {
		if hMouse != 0 {
			procUnhookWindowsHookEx.Call(hMouse)
		}
		if hKey != 0 {
			procUnhookWindowsHookEx.Call(hKey)
		}
	}()

	var m msg
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if r == 0 || r == ^uintptr(0) {
			return
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func wndProc(hwnd, uMsg, wParam, lParam uintptr) uintptr {
	if uMsg == wmInput {
		handleRawInput(lParam)
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, uMsg, wParam, lParam)
	return r
}

func handleRawInput(lParam uintptr) {
	if cbs.OnMouseMove == nil {
		return
	}
	var buf [128]byte
	size := uint32(len(buf))
	r, _, _ := procGetRawInputData.Call(lParam, ridInput,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)),
		uintptr(unsafe.Sizeof(rawinputHeader{})))
	if r == ^uintptr(0) || size < uint32(unsafe.Sizeof(rawmouse{})) {
		return
	}
	h := (*rawinputHeader)(unsafe.Pointer(&buf[0]))
	if h.dwType != 0 { // 只处理鼠标
		return
	}
	m := (*rawmouse)(unsafe.Pointer(&buf[unsafe.Sizeof(rawinputHeader{})]))
	if m.usFlags&1 != 0 { // MOUSE_MOVE_ABSOLUTE，忽略
		return
	}
	cbs.OnMouseMove(int(m.lLastX), int(m.lLastY))
}

func mouseHookProc(nCode int, wParam, lParam uintptr) uintptr {
	if nCode >= 0 {
		ms := (*msllhookstruct)(unsafe.Pointer(lParam))
		deliver := !suppress.Load() || ms.flags&llmhfInjected != 0
		switch wParam {
		case wmLButtonDown, wmLButtonUp, wmRButtonDown, wmRButtonUp,
			wmMButtonDown, wmMButtonUp, wmXButtonDown, wmXButtonUp:
			if cbs.OnMouseButton != nil {
				down := wParam == wmLButtonDown || wParam == wmRButtonDown ||
					wParam == wmMButtonDown || wParam == wmXButtonDown
				btn := 0
				switch wParam {
				case wmLButtonDown, wmLButtonUp:
					btn = 1
				case wmRButtonDown, wmRButtonUp:
					btn = 2
				case wmMButtonDown, wmMButtonUp:
					btn = 3
				default:
					btn = 4 + int(uint32(uint16(ms.mouseData>>16))-1)
				}
				cbs.OnMouseButton(down, btn, int(ms.x), int(ms.y))
			}
		case wmMouseWheel, wmMouseHWheel:
			if cbs.OnWheel != nil {
				cbs.OnWheel(int32(int16(ms.mouseData>>16)), wParam == wmMouseHWheel)
			}
		}
		if !deliver {
			return 1 // 吞掉，不传给本机
		}
	}
	r, _, _ := procCallNextHookEx.Call(0, uintptr(nCode), wParam, lParam)
	return r
}

func keyHookProc(nCode int, wParam, lParam uintptr) uintptr {
	if nCode >= 0 {
		kb := (*kbdllhookstruct)(unsafe.Pointer(lParam))
		if cbs.OnKey != nil {
			cbs.OnKey(kb.vkCode, kb.scanCode, kb.flags&llkhfUp == 0, kb.flags&llkhfExtended != 0)
		}
		if suppress.Load() && kb.flags&llkhfInjected == 0 {
			return 1 // 吞掉本地键盘
		}
	}
	r, _, _ := procCallNextHookEx.Call(0, uintptr(nCode), wParam, lParam)
	return r
}

func sendMouse(flags uint32, dx, dy int32, data int32) {
	var in inputStruct
	in.typ = inputMouse
	mi := mouseInput{dx: dx, dy: dy, mouseData: uint32(data), dwFlags: flags}
	copy(in.u[:], unsafe.Slice((*byte)(unsafe.Pointer(&mi)), unsafe.Sizeof(mi)))
	procSendInput.Call(1, uintptr(unsafe.Pointer(&in)), uintptr(unsafe.Sizeof(in)))
}

func sendKey(vk uint32, scan uint16, flags uint32) {
	var in inputStruct
	in.typ = inputKeyboard
	ki := keybdInput{wVk: uint16(vk), wScan: scan, dwFlags: flags}
	copy(in.u[:], unsafe.Slice((*byte)(unsafe.Pointer(&ki)), unsafe.Sizeof(ki)))
	procSendInput.Call(1, uintptr(unsafe.Pointer(&in)), uintptr(unsafe.Sizeof(in)))
}
