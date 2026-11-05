package main

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// 托盘图标是由 systray 创建的一个隐藏窗口（窗口类 SystrayClass）提交给系统的。
// 因此"窗口在不在"是比注册表登记更可靠的判据：窗口在 = 托盘代码确实跑到了，
// 图标已交给资源管理器；窗口不在 = 进程根本没走到托盘那一步（被拦住/卡住/报错）。
const systrayClassName = "SystrayClass"

var (
	user32              = windows.NewLazySystemDLL("user32.dll")
	procEnumWindows     = user32.NewProc("EnumWindows")
	procGetClassNameW   = user32.NewProc("GetClassNameW")
	procGetWindowPID    = user32.NewProc("GetWindowThreadProcessId")
	procIsWindowVisible = user32.NewProc("IsWindowVisible")
)

// trayWindow 描述一个疑似托盘窗口。
type trayWindow struct {
	hwnd    uintptr
	pid     uint32
	visible bool
	exeName string
}

// findSystrayWindows 枚举本会话里的系统托盘窗口。
func findSystrayWindows() []trayWindow {
	var out []trayWindow
	cb := syscall.NewCallback(func(hwnd uintptr, _ uintptr) uintptr {
		var buf [128]uint16
		n, _, _ := procGetClassNameW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
		if n == 0 {
			return 1
		}
		if windows.UTF16ToString(buf[:n]) != systrayClassName {
			return 1
		}
		var pid uint32
		procGetWindowPID.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
		vis, _, _ := procIsWindowVisible.Call(hwnd)
		out = append(out, trayWindow{hwnd: hwnd, pid: pid, visible: vis != 0, exeName: processName(pid)})
		return 1
	})
	procEnumWindows.Call(cb, 0)
	return out
}

// processName 返回进程的可执行文件名（取不到时返回空串）。
func processName(pid uint32) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_PATH)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return ""
	}
	full := windows.UTF16ToString(buf[:size])
	for i := len(full) - 1; i >= 0; i-- {
		if full[i] == '\\' || full[i] == '/' {
			return full[i+1:]
		}
	}
	return full
}
