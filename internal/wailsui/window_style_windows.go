//go:build windows

package wailsui

import (
	"errors"
	"fmt"
	"time"
)

// 吸边图标需要的原生窗口增强（Windows）。
//
// 为什么必须动样式位：带 WS_CAPTION / WS_SYSMENU 的顶层窗口，Windows 会把
// 窗口宽度撑到 SM_CXMIN 左右（实测 131px），即使 WM_GETMINMAXINFO 里把
// 上限设成 64 也没用。结果是「收起」后窗口变成一条 131x52 的横条，贴边时
// 图标被挤到屏幕外，用户看到的就是"没有图标"。

const (
	gwlStyle      = -16
	wsCaption     = 0x00C00000
	wsSysMenu     = 0x00080000
	wsMinimizeBox = 0x00020000
	wsMaximizeBox = 0x00010000

	wmNCLButtonDown = 0x00A1
	htCaption       = 2

	swpNoSize       = 0x0001
	swpNoMove       = 0x0002
	swpNoZOrder     = 0x0004
	swpNoActivate   = 0x0010
	swpFrameChanged = 0x0020
)

var (
	procGetWindowLongPtrW  = user32.NewProc("GetWindowLongPtrW")
	procSetWindowLongPtrW  = user32.NewProc("SetWindowLongPtrW")
	procSetWindowPosW      = user32.NewProc("SetWindowPos")
	procGetAsyncKeyState   = user32.NewProc("GetAsyncKeyState")
	procGetDoubleClickTime = user32.NewProc("GetDoubleClickTime")
	procReleaseCapture     = user32.NewProc("ReleaseCapture")
	procPostMessageW       = user32.NewProc("PostMessageW")
)

// trayMenuDelay 返回单击托盘后等待“可能是双击”的时间：取系统双击间隔，
// 夹在 200–350ms 之间——太短会把双击吃成两次单击，太长会让单击菜单发木。
func trayMenuDelay() time.Duration {
	v, _, _ := procGetDoubleClickTime.Call()
	ms := int(v)
	if ms < 200 {
		ms = 200
	}
	if ms > 350 {
		ms = 350
	}
	return time.Duration(ms) * time.Millisecond
}

const vkLButton = 0x01

// leftMouseDown 报告左键是否仍按着：拖动窗口的过程中不要抢先吸边收起，
// 等用户松手（或拖动结束后）再判断，避免面板在手里"突然缩成图标"。
func leftMouseDown() bool {
	v, _, _ := procGetAsyncKeyState.Call(vkLButton)
	return uint16(v)&0x8000 != 0
}

func getWindowLongPtr(hwnd uintptr, index int32) uintptr {
	v, _, _ := procGetWindowLongPtrW.Call(hwnd, uintptr(index))
	return v
}

func setWindowLongPtr(hwnd uintptr, index int32, value uintptr) {
	_, _, _ = procSetWindowLongPtrW.Call(hwnd, uintptr(index), value)
}

// notifyFrameChanged 让窗口按新样式重算非客户区（与 Wails 自己改样式后的做法一致）。
func notifyFrameChanged(hwnd uintptr) {
	_, _, _ = procSetWindowPosW.Call(hwnd, 0, 0, 0, 0, 0,
		swpNoMove|swpNoSize|swpNoZOrder|swpNoActivate|swpFrameChanged)
}

// setFramelessDockStyle 去掉标题栏/系统菜单/最小化最大化按钮：
// 只有这样窗口才能缩到 64x64 的方形图标，且不会因系统最小宽度被撑开。
func setFramelessDockStyle(hwnd uintptr) {
	style := getWindowLongPtr(hwnd, gwlStyle)
	style &^= wsCaption | wsSysMenu | wsMinimizeBox | wsMaximizeBox
	setWindowLongPtr(hwnd, gwlStyle, style)
	notifyFrameChanged(hwnd)
}

// restoreCaptionStyle 回到标准界面时把标题栏与按钮位加回来。
func restoreCaptionStyle(hwnd uintptr) {
	style := getWindowLongPtr(hwnd, gwlStyle)
	style |= wsCaption | wsSysMenu | wsMinimizeBox | wsMaximizeBox
	setWindowLongPtr(hwnd, gwlStyle, style)
	notifyFrameChanged(hwnd)
}

// startWindowDrag 让窗口进入系统的原生拖动循环（与 Wails 自带拖动区同一做法）。
// 用 PostMessage：WM_NCLBUTTONDOWN 会让 DefWindowProc 进入模态移动循环，
// SendMessage 会阻塞到用户松手为止。
func startWindowDrag(hwnd uintptr) error {
	if hwnd == 0 {
		return errors.New("窗口尚未就绪")
	}
	_, _, _ = procReleaseCapture.Call()
	if r, _, err := procPostMessageW.Call(hwnd, wmNCLButtonDown, htCaption, 0); r == 0 {
		return fmt.Errorf("发送拖动消息失败: %w", err)
	}
	return nil
}
