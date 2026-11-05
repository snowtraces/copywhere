package app

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"copywhere/internal/bytesize"
	"copywhere/internal/shellmenu"
)

// shellBatchWindow 是右键菜单发送的合并窗口（静默期）。
//
// 资源管理器对多选文件按"每项调用一次"分发（MultiSelectModel 的 Document 语义），
// 每次唤起一个短进程只带一个 %1；窗口把同一批选择拼成一次发送，
// 对端因此只收到一个 zip 包，而不是 N 条独立消息与 N 次节点扫描。
const shellBatchWindow = 400 * time.Millisecond

// shellBatchMaxWait 是同一批次的硬上界：每来一个新的路径就重新静默计时，
// 但首批投递起最多等这么久——无论进程唤起快慢，选择多大都不会无限等下去。
const shellBatchMaxWait = 1500 * time.Millisecond

// ShellSend 接收资源管理器右键菜单交来的路径（面板 /api/send-paths 调用）。
// 不存在的路径（用户刚删掉/移动了文件）直接跳过并记日志。
func (a *App) ShellSend(paths []string) {
	valid := make([]string, 0, len(paths))
	for _, p := range paths {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err != nil {
			log.Printf("右键菜单发送跳过不可访问的路径 %s: %v", p, err)
			continue
		}
		valid = append(valid, p)
	}
	a.shellBatcher.Add(valid)
}

// shellSendFiles 在合并窗口结束后执行真正的发送（与面板/CLI 手动发送同一通道，
// 不受自动同步阈值限制）。
func (a *App) shellSendFiles(paths []string) {
	var total int64
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			total += fi.Size()
		}
	}
	log.Printf("资源管理器右键菜单：发送 %d 项（%s）到全部在线节点：%s",
		len(paths), bytesize.Human(total), shellPathsPreview(paths))
	a.SendFiles(paths, total)
}

// shellPathsPreview 把路径列表压成一行日志（最多 3 个名字，其余以 +N 省略）。
func shellPathsPreview(paths []string) string {
	const maxNames = 3
	names := make([]string, 0, maxNames)
	for i, p := range paths {
		if i == maxNames {
			break
		}
		names = append(names, filepath.Base(p))
	}
	if rest := len(paths) - len(names); rest > 0 {
		names = append(names, fmt.Sprintf("+%d", rest))
	}
	return strings.Join(names, "、")
}

// SetShellMenu 注册/注销资源管理器右键菜单（「用 copywhere 发送」）。
// gui 启动与面板开关都走这里：菜单命令指向当前 exe，程序被移动或升级后
// 启动时自动纠正（Installed 按命令行比对，指向别处即重新注册）。
func (a *App) SetShellMenu(on bool) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("定位当前程序路径失败: %w", err)
	}
	if on {
		if shellmenu.Installed(exe) {
			return nil // 已指向当前 exe，无需每次启动都写注册表
		}
		if err := shellmenu.Install(exe); err != nil {
			return err
		}
		log.Printf("已注册资源管理器右键菜单「%s」（可在面板「设置 → 通用」关闭）", shellmenu.MenuText)
		return nil
	}
	// 关闭时无条件清理：Install 是逐个 target 写键的，杀软中途拦截会留下
	// "文件有菜单、文件夹没有"的半注册状态，而 Installed("") 要求两个 target
	// 都在——用它做守卫，用户就会关不掉残留菜单项。Uninstall 本身幂等、
	// 对缺失键容错（registry.ErrNotExist 视为成功）。
	registered := shellmenu.Installed("") // 仅用于决定是否记日志
	if err := shellmenu.Uninstall(); err != nil {
		return err
	}
	if registered {
		log.Printf("已从资源管理器右键菜单移除「%s」", shellmenu.MenuText)
	}
	return nil
}

// pathBatcher 把短时间内的多次投递合并成一次回调。
// 计时是"静默 window 后触发，但首批投递起最多等 maxWait"：
// 选择项多、进程唤起慢时也能尽量并入同一批，同时保证不会无限等下去。
type pathBatcher struct {
	window  time.Duration
	maxWait time.Duration
	fn      func([]string)

	mu      sync.Mutex
	pending []string
	first   time.Time // 本批次首个投递时刻（maxWait 从它起算）
	timer   *time.Timer
}

// newPathBatcher 创建合并器：窗口到期后用攒下的路径调用一次 fn。
func newPathBatcher(window, maxWait time.Duration, fn func([]string)) *pathBatcher {
	if maxWait < window {
		maxWait = window
	}
	return &pathBatcher{window: window, maxWait: maxWait, fn: fn}
}

// Add 追加路径（同批次内去重）并重排计时。
func (b *pathBatcher) Add(paths []string) {
	if len(paths) == 0 {
		return
	}
	now := time.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.pending) == 0 {
		b.first = now
	}
	for _, p := range paths {
		// 批量很小（一次右键选择），线性查重足够且不必维护额外索引
		if !slices.Contains(b.pending, p) {
			b.pending = append(b.pending, p)
		}
	}
	wait := b.window
	if remain := b.maxWait - now.Sub(b.first); remain < wait {
		wait = remain
	}
	if wait < 0 {
		wait = 0
	}
	if b.timer == nil {
		b.timer = time.AfterFunc(wait, b.flush)
	} else {
		b.timer.Reset(wait)
	}
}

// flush 取走当前批次并回调；空批次不触发回调。
func (b *pathBatcher) flush() {
	b.mu.Lock()
	paths := b.pending
	b.pending = nil
	b.timer = nil
	b.mu.Unlock()
	if len(paths) == 0 {
		return
	}
	b.fn(paths)
}
