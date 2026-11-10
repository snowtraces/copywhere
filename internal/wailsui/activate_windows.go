package wailsui

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// windowTitle 是 Wails 主窗口标题；激活已有窗口时按它查找（必须与
// app.go 中 WebviewWindowOptions.Title 保持一致）。
const windowTitle = "copywhere"

var (
	user32                    = windows.NewLazySystemDLL("user32.dll")
	kernel32                  = windows.NewLazySystemDLL("kernel32.dll")
	procEnumWindows           = user32.NewProc("EnumWindows")
	procGetWindowThreadPID    = user32.NewProc("GetWindowThreadProcessId")
	procGetWindowTextW        = user32.NewProc("GetWindowTextW")
	procGetClassNameW         = user32.NewProc("GetClassNameW")
	procShowWindow            = user32.NewProc("ShowWindow")
	procSetForegroundWindow   = user32.NewProc("SetForegroundWindow")
	procQueryFullProcessImage = kernel32.NewProc("QueryFullProcessImageNameW")
)

// ActivateExistingWindow 查找已运行的 copywhere Wails 窗口并置前。
//
// 单实例保护在 cmd 层（命名互斥锁）：第二次启动时进程会主动退出，无法让
// Wails 的 second-instance 回调生效；这里枚举顶层窗口，按
// 「WailsWebviewWindow 类 + title + 同 exe 名」定位原窗口，恢复并置前。
// 只按标题查会误中 explorer 托管的同名影子窗口，因此必须校验所属进程。
func ActivateExistingWindow() bool {
	own := ownExeBase()
	if own == "" {
		return false
	}
	var (
		mu    sync.Mutex
		found uintptr
	)
	cb := syscall.NewCallback(func(hwnd, _ uintptr) uintptr {
		if found != 0 || !isOurWindow(hwnd, own) {
			return 1 // 继续枚举
		}
		mu.Lock()
		found = hwnd
		mu.Unlock()
		return 0 // 找到了，停止枚举
	})
	procEnumWindows.Call(cb, 0)

	if found == 0 {
		return false
	}
	const swRestore = 9 // SW_RESTORE
	procShowWindow.Call(found, swRestore)
	procSetForegroundWindow.Call(found)
	return true
}

// isOurWindow 判断 hwnd 是否是本程序的 Wails 主窗口。
func isOurWindow(hwnd uintptr, ownExe string) bool {
	var pid uint32
	procGetWindowThreadPID.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	if pid == 0 {
		return false
	}
	if windowText(hwnd) != windowTitle {
		return false
	}
	if windowClass(hwnd) != "WailsWebviewWindow" {
		return false
	}
	return strings.EqualFold(processExeBase(pid), ownExe)
}

func windowText(hwnd uintptr) string {
	var buf [256]uint16
	n, _, _ := procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 {
		return ""
	}
	return windows.UTF16ToString(buf[:n])
}

func windowClass(hwnd uintptr) string {
	var buf [256]uint16
	n, _, _ := procGetClassNameW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 {
		return ""
	}
	return windows.UTF16ToString(buf[:n])
}

// ownExeBase 返回当前进程 exe 的文件名（不含路径）。
func ownExeBase() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Base(exe)
}

// processExeBase 返回指定进程的 exe 文件名；无权限/已退出时返回空串。
func processExeBase(pid uint32) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	var buf [windows.MAX_LONG_PATH]uint16
	n := uint32(len(buf))
	r, _, _ := procQueryFullProcessImage.Call(
		uintptr(h), 0,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&n)),
	)
	if r == 0 || n == 0 {
		return ""
	}
	return filepath.Base(windows.UTF16ToString(buf[:n]))
}
