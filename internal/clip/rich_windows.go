//go:build windows

package clip

import (
	"errors"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"

	"copywhere/internal/diag"
)

// formatRTF / formatHTML 是需注册的剪贴板格式名（Windows 标准命名）。
const (
	formatRTF  = "Rich Text Format"
	formatHTML = "HTML Format"
)

var (
	cfRichText   uintptr // init 中登记；0 表示注册失败
	cfHTMLFormat uintptr
)

// maxRichBytes 是单一富格式的**原始字节**读取上限（32MB）：HTML/RTF 正常在
// MB 级以内，超限视为异常来源，只保留纯文本，避免拖垮内存与传输。
// 注意与 transport.MaxRichBytes（整个 rich JSON 负载 16MB，[]byte 经 JSON
// 为 base64，膨胀 4/3）是两个不同口径的闸：这里超限丢弃单个格式，发送端
// 超限整体降级纯文本。
const maxRichBytes = 32 << 20

func init() {
	if p, err := windows.UTF16PtrFromString(formatRTF); err == nil {
		cfRichText, _, _ = procRegisterClipboardFormat.Call(uintptr(unsafe.Pointer(p)))
	}
	if p, err := windows.UTF16PtrFromString(formatHTML); err == nil {
		cfHTMLFormat, _, _ = procRegisterClipboardFormat.Call(uintptr(unsafe.Pointer(p)))
	}
}

// readFormatLocked 读取指定格式为字节（须在剪贴板线程、已 OpenClipboard 时调用）。
// 格式缺失/超限/锁定失败一律返回 ok=false，调用方按"无此格式"处理。
func readFormatLocked(fmtID uintptr) ([]byte, bool) {
	if fmtID == 0 {
		return nil, false
	}
	if r, _, _ := procIsClipboardFormatAvailable.Call(fmtID); r == 0 {
		return nil, false
	}
	h, ok := getClipboardData(fmtID)
	if !ok {
		// 系统声称格式存在却取不到数据：不是"无此格式"，记入诊断环，
		// 避免嵌套开关剪贴板这类所有权错误被静默吞掉。
		diag.Incr("clip.format_unexpected")
		return nil, false
	}
	size, _, _ := syscall.Syscall(procGlobalSize.Addr(), 1, h, 0, 0)
	if size == 0 {
		return nil, false
	}
	if size > maxRichBytes {
		// 超限丢弃该格式是有意降级，但留计数便于诊断异常来源
		diag.Incr("clip.format_toobig")
		return nil, false
	}
	p, ok := globalLock(h)
	if !ok {
		diag.Incr("clip.format_unexpected")
		return nil, false
	}
	defer procGlobalUnlock.Call(h)
	return append([]byte(nil), unsafe.Slice((*byte)(p), size)...), true
}

// ReadRich 读取带格式的剪贴板文本：CF_UNICODETEXT + HTML Format + Rich Text Format。
// 无文本时返回 ErrNoText（富格式必须有文本兜底，纯格式无文本不予搬运）。
func ReadRich() (Rich, error) {
	var rich Rich
	var err error
	runClipboard(func() { rich, err = readRichLocked() })
	return rich, err
}

func readRichLocked() (Rich, error) {
	rich := Rich{}
	if err := openClipboard(); err != nil {
		return rich, err
	}
	defer procCloseClipboard.Call()

	text, terr := readTextFromOpenLocked()
	if terr != nil {
		return rich, terr
	}
	rich.Text = text
	// 读取失败均静默按"无此格式"处理，但格式存在（available 通过）却拿不到
	// 数据、或魔数不符的情况记入诊断环，避免故障被静默吞掉。
	if b, ok := readFormatLocked(cfHTMLFormat); ok {
		if looksLikeHTML(b) {
			rich.HTML = trimNUL(b)
		} else {
			diag.Incr("clip.read_rich_magic_reject")
		}
	}
	if b, ok := readFormatLocked(cfRichText); ok {
		if looksLikeRTF(b) {
			rich.RTF = trimNUL(b)
		} else {
			diag.Incr("clip.read_rich_magic_reject")
		}
	}
	return rich, nil
}

// looksLikeHTML / looksLikeRTF 是富格式轻校验：魔数不符即视为来源异常
// （仿冒格式名的垃圾数据），丢弃该格式仅保留纯文本，避免把畸形内容
// 原样递给对端应用的 HTML/RTF 解析器。
func looksLikeHTML(b []byte) bool {
	// CF_HTML 规范以 "Version:" 行开头（可带 BOM）
	s := strings.TrimPrefix(string(b), "\ufeff")
	return strings.HasPrefix(s, "Version:")
}

func looksLikeRTF(b []byte) bool {
	s := strings.TrimPrefix(string(b), "\ufeff")
	return strings.HasPrefix(s, `{\rtf`) || strings.HasPrefix(s, `{\*\rtf`)
}

// trimNUL 去掉尾部填充的 null 字节（剪贴板数据常以 \0 结尾）。
func trimNUL(b []byte) []byte {
	for len(b) > 0 && b[len(b)-1] == 0 {
		b = b[:len(b)-1]
	}
	return b
}

// SetRich 一次性把多种格式写回剪贴板：纯文本必写，HTML/RTF 有则附带。
// 与逐个调用 SetText/SetClipboardData 的关键区别是只 EmptyClipboard 一次——
// 分次写入会让前一次被后一次清空，最终只剩最后一种格式。
func SetRich(r Rich) error {
	if r.Text == "" {
		return errors.New("SetRich: 纯文本为空")
	}
	// 写入侧同样做魔数轻校验（读取侧在 readRichLocked）：远端数据经
	// onRichReceived 走到这里，不符合 HTML/RTF 特征的内容一律降级丢弃，
	// 绝不把可疑字节递给本机应用的解析器。
	if len(r.HTML) > 0 && !looksLikeHTML(r.HTML) {
		diag.Incr("clip.set_rich_html_rejected")
		r.HTML = nil
	}
	if len(r.RTF) > 0 && !looksLikeRTF(r.RTF) {
		diag.Incr("clip.set_rich_rtf_rejected")
		r.RTF = nil
	}
	text, err := utf16LEBytes(r.Text)
	if err != nil {
		return err
	}
	var runErr error
	runClipboard(func() { runErr = setRichLocked(text, r) })
	return runErr
}

func setRichLocked(text []byte, r Rich) error {
	type item struct {
		id   uintptr
		data []byte
	}
	items := []item{{cfUnicodeText, text}}
	if len(r.HTML) > 0 && cfHTMLFormat != 0 {
		items = append(items, item{cfHTMLFormat, r.HTML})
	}
	if len(r.RTF) > 0 && cfRichText != 0 {
		items = append(items, item{cfRichText, r.RTF})
	}

	// 阶段一：全部分配并填充。此刻尚未打开剪贴板，任一失败都自行清理，零副作用。
	handles := make([]uintptr, len(items))
	defer func() {
		for _, h := range handles {
			if h != 0 {
				procGlobalFree.Call(h)
			}
		}
	}()
	for i, it := range items {
		h, ok := globalAlloc(gmemMoveable, uintptr(len(it.data)))
		if !ok {
			return errors.New("GlobalAlloc 失败")
		}
		p, ok := globalLock(h)
		if !ok {
			return errors.New("GlobalLock 失败")
		}
		copy(unsafe.Slice((*byte)(p), len(it.data)), it.data)
		procGlobalUnlock.Call(h)
		handles[i] = h
	}

	// 阶段二：打开剪贴板、清空、逐格式交接。
	if err := openClipboard(); err != nil {
		return err
	}
	defer procCloseClipboard.Call()
	procEmptyClipboard.Call()
	for i, it := range items {
		if setClipboardData(it.id, handles[i]) {
			handles[i] = 0 // 内存已归系统所有，本函数不得再释放
			continue
		}
		// 交接失败：句柄仍属本进程，保持原值由 defer 统一 GlobalFree；
		// 已成功交接的前序格式留在剪贴板里，CloseClipboard 后系统照常
		// 持有——剪贴板处于"部分写入"状态，比整个操作半途崩溃更安全。
		return errors.New("SetClipboardData 失败")
	}
	return nil
}
