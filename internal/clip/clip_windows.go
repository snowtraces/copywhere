//go:build windows

// Package clip 封装 Windows 剪贴板读写（CF_UNICODETEXT / CF_HDROP）。
//
// 注意：本文件中 GlobalLock 返回的 uintptr → unsafe.Pointer 转换是 Win32
// 内存句柄的标准用法（指针仅在 GlobalLock/GlobalUnlock 之间短生命周期使用），
// go vet 的 unsafeptr 检查器无法识别该模式，会报 "possible misuse of
// unsafe.Pointer"，因此本项目的 vet 门禁使用 `go vet -unsafeptr=false`。
package clip

import (
	"encoding/binary"
	"errors"
	"fmt"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	cfUnicodeText = 13
	cfHDROP       = 15
	gmemMoveable  = 0x0002
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procOpenClipboard              = user32.NewProc("OpenClipboard")
	procCloseClipboard             = user32.NewProc("CloseClipboard")
	procEmptyClipboard             = user32.NewProc("EmptyClipboard")
	procGetClipboardData           = user32.NewProc("GetClipboardData")
	procSetClipboardData           = user32.NewProc("SetClipboardData")
	procGetClipboardSequenceNumber = user32.NewProc("GetClipboardSequenceNumber")
	procGlobalAlloc                = kernel32.NewProc("GlobalAlloc")
	procGlobalLock                 = kernel32.NewProc("GlobalLock")
	procGlobalUnlock               = kernel32.NewProc("GlobalUnlock")
	procGlobalFree                 = kernel32.NewProc("GlobalFree")
)

var (
	// ErrNoFiles 表示剪贴板当前没有文件列表（CF_HDROP）。
	ErrNoFiles = errors.New("剪贴板中没有文件列表 (CF_HDROP)")
	// ErrNoText 表示剪贴板当前没有文本（CF_UNICODETEXT）。
	ErrNoText = errors.New("剪贴板中没有文本 (CF_UNICODETEXT)")
)

// dropfiles 对应 Win32 DROPFILES 结构。
type dropfiles struct {
	pFiles uint32
	pt     [2]int32
	fNC    int32
	fWide  int32
}

// Seq 返回剪贴板序号，内容每次变化都会递增。
func Seq() uint32 {
	r, _, _ := procGetClipboardSequenceNumber.Call()
	return uint32(r)
}

func openClipboard() error {
	var last error
	for i := 0; i < 20; i++ {
		r, _, _ := procOpenClipboard.Call(0)
		if r != 0 {
			return nil
		}
		last = errors.New("OpenClipboard 失败（被其他进程占用）")
		time.Sleep(25 * time.Millisecond)
	}
	return last
}

// 通过 syscall.Syscall 包装 GlobalAlloc/GlobalLock，返回类型化指针。
func globalAlloc(flags, size uintptr) (uintptr, bool) {
	r, _, _ := syscall.Syscall(procGlobalAlloc.Addr(), 2, flags, size, 0)
	return r, r != 0
}

func globalLock(h uintptr) (unsafe.Pointer, bool) {
	r, _, _ := syscall.Syscall(procGlobalLock.Addr(), 1, h, 0, 0)
	return unsafe.Pointer(r), r != 0
}

func getClipboardData(format uintptr) (uintptr, bool) {
	r, _, _ := syscall.Syscall(procGetClipboardData.Addr(), 1, format, 0, 0)
	return r, r != 0
}

func setClipboardData(format uintptr, h uintptr) bool {
	r, _, _ := syscall.Syscall(procSetClipboardData.Addr(), 2, format, h, 0)
	return r != 0
}

// ReadFiles 读取剪贴板中的文件路径列表（Unicode DROPFILES）。
func ReadFiles() ([]string, error) {
	if err := openClipboard(); err != nil {
		return nil, err
	}
	defer procCloseClipboard.Call()

	h, ok := getClipboardData(cfHDROP)
	if !ok {
		return nil, ErrNoFiles
	}
	p, ok := globalLock(h)
	if !ok {
		return nil, errors.New("GlobalLock 失败")
	}
	defer procGlobalUnlock.Call(h)

	df := (*dropfiles)(p)
	if df.fWide == 0 {
		return nil, ErrNoFiles // ANSI 格式，罕见，暂不支持
	}
	cur := unsafe.Add(p, uintptr(df.pFiles))
	var out []string
	var chars []uint16
	for i := 0; i < 1<<20; i++ {
		ch := *(*uint16)(cur)
		cur = unsafe.Add(cur, 2)
		if ch == 0 {
			if len(chars) == 0 {
				break // 双 null 结束
			}
			out = append(out, syscall.UTF16ToString(chars))
			chars = chars[:0]
			continue
		}
		chars = append(chars, ch)
	}
	if len(out) == 0 {
		return nil, ErrNoFiles
	}
	return out, nil
}

// ReadText 读取剪贴板文本。
func ReadText() (string, error) {
	if err := openClipboard(); err != nil {
		return "", err
	}
	defer procCloseClipboard.Call()

	h, ok := getClipboardData(cfUnicodeText)
	if !ok {
		return "", ErrNoText
	}
	p, ok := globalLock(h)
	if !ok {
		return "", errors.New("GlobalLock 失败")
	}
	defer procGlobalUnlock.Call(h)

	var chars []uint16
	cur := p
	for i := 0; i < 1<<20; i++ {
		ch := *(*uint16)(cur)
		if ch == 0 {
			break
		}
		chars = append(chars, ch)
		cur = unsafe.Add(cur, 2)
	}
	if len(chars) == 0 {
		return "", ErrNoText
	}
	return syscall.UTF16ToString(chars), nil
}

// SetFiles 将文件路径列表写入剪贴板（CF_HDROP）。成功后系统接管内存。
func SetFiles(paths []string) error {
	if len(paths) == 0 {
		return errors.New("SetFiles: 路径为空")
	}
	wb := make([]byte, 0, 4096)
	for _, p := range paths {
		u, err := syscall.UTF16FromString(p)
		if err != nil {
			return fmt.Errorf("非法路径 %q: %w", p, err)
		}
		for _, v := range u {
			wb = append(wb, byte(v), byte(v>>8))
		}
	}
	wb = append(wb, 0, 0) // 列表结束的双 null

	header := make([]byte, 20)
	binary.LittleEndian.PutUint32(header[0:4], 20) // pFiles 偏移
	// pt (0,0)
	binary.LittleEndian.PutUint32(header[16:20], 1) // fWide = TRUE
	data := append(header, wb...)

	return setClipboardBuffer(cfHDROP, data)
}

// SetText 将文本写入剪贴板（CF_UNICODETEXT）。
func SetText(s string) error {
	u, err := syscall.UTF16FromString(s)
	if err != nil {
		return err
	}
	data := make([]byte, len(u)*2)
	for i, v := range u {
		binary.LittleEndian.PutUint16(data[i*2:], v)
	}
	return setClipboardBuffer(cfUnicodeText, data)
}

func setClipboardBuffer(format uintptr, data []byte) error {
	h, ok := globalAlloc(gmemMoveable, uintptr(len(data)))
	if !ok {
		return errors.New("GlobalAlloc 失败")
	}
	p, ok := globalLock(h)
	if !ok {
		procGlobalFree.Call(h)
		return errors.New("GlobalLock 失败")
	}
	copy(unsafe.Slice((*byte)(p), len(data)), data)
	procGlobalUnlock.Call(h)

	if err := openClipboard(); err != nil {
		procGlobalFree.Call(h)
		return err
	}
	procEmptyClipboard.Call()
	ok = setClipboardData(format, h)
	procCloseClipboard.Call()
	if !ok {
		procGlobalFree.Call(h)
		return errors.New("SetClipboardData 失败")
	}
	return nil // 成功后内存归系统所有
}
