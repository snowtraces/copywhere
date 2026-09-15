//go:build windows

// Package clip 封装 Windows 剪贴板读写（CF_UNICODETEXT / CF_HDROP）。
//
// 注意：本文件中 GlobalLock 返回的 uintptr → unsafe.Pointer 转换是 Win32
// 内存句柄的标准用法（指针仅在 GlobalLock/GlobalUnlock 之间短生命周期使用），
// go vet 的 unsafeptr 检查器无法识别该模式，会报 "possible misuse of
// unsafe.Pointer"，因此本项目的 vet 门禁使用 `go vet -unsafeptr=false`。
package clip

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/png"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	cfUnicodeText = 13
	cfHDROP       = 15
	cfDIB         = 8
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
	procIsClipboardFormatAvailable = user32.NewProc("IsClipboardFormatAvailable")
	procRegisterClipboardFormat    = user32.NewProc("RegisterClipboardFormatW")
	procGlobalAlloc                = kernel32.NewProc("GlobalAlloc")
	procGlobalLock                 = kernel32.NewProc("GlobalLock")
	procGlobalUnlock               = kernel32.NewProc("GlobalUnlock")
	procGlobalFree                 = kernel32.NewProc("GlobalFree")
	procGlobalSize                 = kernel32.NewProc("GlobalSize")
)

// cfPNG 是 "PNG" 注册剪贴板格式（截图工具等应用会直接放入现成 PNG）。
var cfPNG uintptr

func init() {
	if p, err := windows.UTF16PtrFromString("PNG"); err == nil {
		r, _, _ := procRegisterClipboardFormat.Call(uintptr(unsafe.Pointer(p)))
		cfPNG = r
	}
	go clipboardThread()
}

var (
	// ErrNoFiles 表示剪贴板当前没有文件列表（CF_HDROP）。
	ErrNoFiles = errors.New("剪贴板中没有文件列表 (CF_HDROP)")
	// ErrNoText 表示剪贴板当前没有文本（CF_UNICODETEXT）。
	ErrNoText = errors.New("剪贴板中没有文本 (CF_UNICODETEXT)")
	// ErrNoImage 表示剪贴板当前没有图片（PNG / CF_DIB）。
	ErrNoImage = errors.New("剪贴板中没有图片 (PNG/CF_DIB)")
)

// ---------- 专用剪贴板线程 ----------
//
// Windows 剪贴板按线程持有：OpenClipboard 把剪贴板锁定到调用线程，
// CloseClipboard 必须由当初 Open 的同一线程执行才生效。Go 调度器不保证
// 同一 goroutine 的两次系统调用落在同一 OS 线程——临界区内一旦抢占/迁移，
// Close 会静默失败，剪贴板被本进程一个再也不会关闭的线程长期占用并逐步
// 累积，表现为「Windows 过一段时间无法复制文件、退出 copywhere 后恢复」
// （进程退出时 OS 回收全部占用）。
//
// 解决办法：所有需要打开/关闭剪贴板的读写，统一投递给这个常驻、且
// 永久锁定在单一 OS 线程上的 worker goroutine 串行执行。这样既保证
// open/close 永远同线程，又天然串行化并发调用（同一时刻只有一个
// OpenClipboard 在途），一举消除争用。Seq 不打开剪贴板，无需经此线程。

// clipRequest 是一次投递到剪贴板线程执行的请求。
type clipRequest struct {
	do   func()          // 在剪贴板线程上执行，结果写入其闭包捕获的变量
	done chan struct{}   // 关闭表示 do 已执行完毕（与调用方建立 happens-before）
}

// clipReq 是串行化队列；非缓冲通道确保同一时刻只有一个请求在途。
var clipReq = make(chan clipRequest)

// clipboardThread 常驻运行，永久锁定单一 OS 线程，逐条执行剪贴板请求。
// 全程不 UnlockOSThread：该 goroutine 一生只做剪贴板工作，其线程被进程
// 持有至退出，符合「固定线程亲和」的标准用法。
func clipboardThread() {
	runtime.LockOSThread()
	for r := range clipReq {
		r.do()
		close(r.done)
	}
}

// runClipboard 在剪贴板线程上同步执行 fn，并等待其完成。
// 通过通道收发建立 happens-before：调用方在 fn 内写入的结果，返回后可安全读取。
func runClipboard(fn func()) {
	r := clipRequest{do: fn, done: make(chan struct{})}
	clipReq <- r
	<-r.done
}

// dropfiles 对应 Win32 DROPFILES 结构。
type dropfiles struct {
	pFiles uint32
	pt     [2]int32
	fNC    int32
	fWide  int32
}

// Seq 返回剪贴板序号，内容每次变化都会递增。
// GetClipboardSequenceNumber 无需打开剪贴板且线程安全，故不经剪贴板线程。
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

// globalData 锁定全局内存句柄并复制出全部字节。
func globalData(h uintptr) ([]byte, bool) {
	p, ok := globalLock(h)
	if !ok {
		return nil, false
	}
	defer procGlobalUnlock.Call(h)
	size, _, _ := syscall.Syscall(procGlobalSize.Addr(), 1, h, 0, 0)
	if size == 0 || size > 1<<31 {
		return nil, false
	}
	return append([]byte(nil), unsafe.Slice((*byte)(p), size)...), true
}

// ReadImage 读取剪贴板中的图片，返回 PNG 编码字节。
// 优先取应用直接放入的 "PNG" 注册格式（系统截图等工具常提供），
// 否则将 CF_DIB 位图就地转换为 PNG。无图片时返回 ErrNoImage。
func ReadImage() ([]byte, error) {
	var img []byte
	var err error
	runClipboard(func() { img, err = readImageLocked() })
	return img, err
}

// readImageLocked 在剪贴板线程上执行读图，open/close 必在同一线程配对。
func readImageLocked() ([]byte, error) {
	if err := openClipboard(); err != nil {
		return nil, err
	}
	defer procCloseClipboard.Call()

	if cfPNG != 0 {
		if r, _, _ := procIsClipboardFormatAvailable.Call(cfPNG); r != 0 {
			if h, ok := getClipboardData(cfPNG); ok {
				if data, ok := globalData(h); ok && len(data) > 8 &&
					bytes.Equal(data[1:4], []byte("PNG")) {
					return data, nil // 已是完整 PNG 文件流
				}
			}
		}
	}

	h, ok := getClipboardData(cfDIB)
	if !ok {
		return nil, ErrNoImage
	}
	data, ok := globalData(h)
	if !ok {
		return nil, errors.New("GlobalLock 失败")
	}
	return dibToPNG(data)
}

// dibToPNG 将 CF_DIB（BITMAPINFOHEADER + 调色板 + 像素）转换为 PNG。
// 支持 BI_RGB / BI_BITFIELDS 的 24/32bpp 不压缩位图（截图场景的全部常见情形）。
func dibToPNG(data []byte) ([]byte, error) {
	if len(data) < 40 {
		return nil, errors.New("DIB 数据过短")
	}
	hdrSize := int(binary.LittleEndian.Uint32(data[0:4]))
	width := int64(int32(binary.LittleEndian.Uint32(data[4:8])))
	height := int64(int32(binary.LittleEndian.Uint32(data[8:12])))
	bpp := int(binary.LittleEndian.Uint16(data[14:16]))
	compression := binary.LittleEndian.Uint32(data[16:20])

	if width <= 0 || height == 0 || width > 1<<15 || height > 1<<15 {
		return nil, fmt.Errorf("DIB 尺寸异常 %dx%d", width, height)
	}
	if compression != 0 && compression != 3 { // BI_RGB / BI_BITFIELDS
		return nil, fmt.Errorf("不支持的 DIB 压缩类型 %d", compression)
	}
	if bpp != 24 && bpp != 32 {
		return nil, fmt.Errorf("不支持的位深 %d", bpp)
	}
	topDown := height < 0
	h := int(height)
	if topDown {
		h = -h
	}

	pixelOff := hdrSize
	if compression == 3 && hdrSize <= 40 {
		pixelOff += 12 // V4 以下头大小的 BI_BITFIELDS：掩码紧跟在头后
	}

	rowBytes := (int(width)*bpp + 31) / 32 * 4
	if pixelOff+rowBytes*h > len(data) {
		return nil, fmt.Errorf("DIB 数据不完整：需要 %d 字节，实际 %d", pixelOff+rowBytes*h, len(data))
	}

	// 32bpp 的 alpha 可能全为 0（很多来源不写 alpha）：先探测，全 0 视为不透明
	hasAlpha := bpp == 32
	if hasAlpha {
		hasAlpha = false
		for y := 0; y < h && !hasAlpha; y++ {
			row := data[pixelOff+y*rowBytes:]
			for x := 0; x < int(width); x++ {
				if row[x*4+3] != 0 {
					hasAlpha = true
					break
				}
			}
		}
	}

	img := image.NewNRGBA(image.Rect(0, 0, int(width), h))
	for y := 0; y < h; y++ {
		srcY := y
		if !topDown {
			srcY = h - 1 - y // DIB 默认自底向上存放
		}
		row := data[pixelOff+srcY*rowBytes:]
		dst := img.Pix[y*img.Stride:]
		for x := 0; x < int(width); x++ {
			ofs := x * bpp / 8
			b, g, r := row[ofs], row[ofs+1], row[ofs+2]
			a := uint8(255)
			if bpp == 32 && hasAlpha {
				a = row[ofs+3]
			}
			dst[x*4], dst[x*4+1], dst[x*4+2], dst[x*4+3] = r, g, b, a
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ReadFiles 读取剪贴板中的文件路径列表（Unicode DROPFILES）。
func ReadFiles() ([]string, error) {
	var files []string
	var err error
	runClipboard(func() { files, err = readFilesLocked() })
	return files, err
}

// readFilesLocked 在剪贴板线程上执行读文件列表。
func readFilesLocked() ([]string, error) {
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
	var text string
	var err error
	runClipboard(func() { text, err = readTextLocked() })
	return text, err
}

// readTextLocked 在剪贴板线程上执行读文本。
func readTextLocked() (string, error) {
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
	var err error
	runClipboard(func() { err = setClipboardBufferLocked(format, data) })
	return err
}

// setClipboardBufferLocked 在剪贴板线程上分配内存并写入剪贴板。
// 打开失败（未进入临界区）时不注册 close-defer、直接释放句柄；一旦打开，
// 关闭一定在同线程执行（本函数即在剪贴板线程上运行）。
func setClipboardBufferLocked(format uintptr, data []byte) error {
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
	defer procCloseClipboard.Call()
	procEmptyClipboard.Call()
	if !setClipboardData(format, h) {
		procGlobalFree.Call(h)
		return errors.New("SetClipboardData 失败")
	}
	return nil // 成功后内存归系统所有
}
