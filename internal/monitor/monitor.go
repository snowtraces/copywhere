// Package monitor 监控本机剪贴板，检测到新的文件/文本后触发同步。
//
// 检测通道（B1：借鉴 MouseWithoutBorders 的事件驱动思路）：
//   - 首选事件驱动：clip.StartWatch 把 message-only 窗口挂进系统剪贴板
//     监听链，内容一变即刻收到 WM_CLIPBOARDUPDATE，检测延迟从平均 ~200ms
//     （400ms 轮询的数学期望）降到亚毫秒级；
//   - 兜底轮询：每 400ms 比较一次 GetClipboardSequenceNumber。监听安装失败、
//     事件意外丢失时仍能发现变化，行为退化为旧版本。
//
// 防回环设计：
//  1. 本进程写剪贴板后记录新的 sequence number（OwnSeq），检测时跳过；
//  2. 刚从远端接收内容后的 3 秒内忽略剪贴板变化（LastReceivedAt 兜底竞态窗口）。
package monitor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"copywhere/internal/bytesize"
	"copywhere/internal/clip"
	"copywhere/internal/diag"
)

// Guard 提供防回环状态与暂停开关。
type Guard struct {
	OwnSeq         *atomic.Uint32 // 本进程最近一次写剪贴板后的序号
	LastReceivedAt *atomic.Int64  // 最近一次收到远端内容的 unix nano
	Paused         *atomic.Bool   // 非 nil 且为 true 时跳过自动同步（内容不累积，恢复后不补发）
}

// Sender 是监控器触发的发送接口（由 app 实现）。
type Sender interface {
	SendFiles(paths []string, total int64)
	SendText(text string)
	// SendRich 同步带格式文本（HTML/RTF 可为空）；由实现方负责能力协商：
	// 对旧版本对端自动降级为纯文本。
	SendRich(r clip.Rich)
	// SendImage 同步剪贴板截图（PNG 字节，由实现方负责落盘与发送）
	SendImage(png []byte)
}

const pollInterval = 400 * time.Millisecond

// Run 阻塞运行剪贴板监控，直到 ctx 结束。
// threshold/textSync 通过 getter 每次检测读取，使面板修改配置后即时生效。
func Run(ctx context.Context, guard Guard, threshold func() int64, textSync func() bool, sender Sender) {
	lastSeq := clip.Seq() // 忽略启动时剪贴板里的既有内容
	var lastFP, lastTextFP, lastImgFP string

	// 事件通道：容量 1 的带缓冲通道 + 非阻塞投递 = 天然合并（coalescing），
	// 变化再密集也只保留一次待检测；检测本身幂等（按 seq/指纹去重）。
	events := make(chan struct{}, 1)
	watched := clip.StartWatch(func() {
		diag.Incr("clip.events")
		select {
		case events <- struct{}{}:
		default: // 已有待处理的信号，合并
		}
	})

	mode := "事件驱动 + 轮询兜底"
	if !watched {
		mode = "轮询"
	}
	log.Printf("剪贴板监控已启动（%s，轮询间隔 %s，自动同步阈值 %s）",
		mode, pollInterval, thresholdLabel(threshold()))
	t := time.NewTicker(pollInterval)
	defer t.Stop()

	// detect 执行一轮完整检测（文件 → 文本 → 截图 优先级），返回是否消耗了
	// 一个新序号。事件与轮询两条触发路径共用，保证语义完全一致。
	detect := func() {
		seq := clip.Seq()
		if seq == lastSeq {
			return // 序号未变：事件与轮询互为去重
		}
		lastSeq = seq

		if guard.Paused != nil && guard.Paused.Load() {
			return // 自动同步已暂停：跳过当前内容，恢复后不补发
		}
		if guard.OwnSeq != nil && seq == guard.OwnSeq.Load() {
			return // 本进程自己写入的
		}
		if guard.LastReceivedAt != nil &&
			time.Since(time.Unix(0, guard.LastReceivedAt.Load())) < 3*time.Second {
			return // 刚收到远端内容，防回环兜底
		}

		// 优先处理文件列表
		if files, err := clip.ReadFiles(); err == nil && len(files) > 0 {
			fp, total, ferr := fingerprint(files)
			if ferr != nil {
				log.Printf("读取剪贴板文件信息失败: %v", ferr)
				return
			}
			if fp == lastFP {
				return // 同一批文件，避免重复发送
			}
			lastFP = fp
			if th := threshold(); th > 0 && total > th {
				log.Printf("剪贴板文件共 %s，超过自动同步阈值 %s，已跳过（可在面板或用 send 命令手动发送）",
					bytesize.Human(total), bytesize.Human(th))
				return
			}
			log.Printf("检测到剪贴板文件 %d 个，共 %s，开始同步", len(files), bytesize.Human(total))
			diag.Mark("monitor") // 发送是多节点同步 IO（可长达数分钟），先把心跳续上
			sender.SendFiles(files, total)
			return
		}

		// 再处理文本：存在有效文本时按文本同步（带位图的文本多为
		// Excel/Word 等复制时附带的预览位图，不应误当截图发送）。
		// 读的是带格式版本：HTML/RTF 一并搬运，两端均支持时对端
		// Ctrl+V 粘贴效果与本机一致；对旧版本由 app 层降级为纯文本。
		if rich, terr := clip.ReadRich(); terr == nil && rich.Text != "" {
			// 去重键是全文（文本+富格式）指纹而非纯文本：序号已变、文本相同
			// 但格式不同（如先纯文后富本复制同一选区）时只看 Text 会静默丢格式。
			// 显式分段哈希（段间 0x00 分隔）：避免拼接产生的边界歧义碰撞。
			h := sha256.New()
			h.Write([]byte(rich.Text))
			h.Write([]byte{0})
			h.Write(rich.HTML)
			h.Write([]byte{0})
			h.Write(rich.RTF)
			fp := hex.EncodeToString(h.Sum(nil)[:8])
			if textSync() && fp != lastTextFP {
				lastTextFP = fp
				if rich.HasFormats() {
					log.Printf("检测到剪贴板富文本 %d 字符（含富格式 %s），开始同步",
						len(rich.Text), bytesize.Human(rich.Size()))
				} else {
					log.Printf("检测到剪贴板文本 %d 字符，开始同步", len(rich.Text))
				}
				sender.SendRich(rich)
			}
			return
		}

		// 最后兜底：无文件无文本，检测截图位图（Win+Shift+S / PrintScreen
		// 等截图直接写入剪贴板、不落盘的情形）
		if img, err := clip.ReadImage(); err == nil && len(img) > 0 {
			sum := sha256.Sum256(img)
			fp := hex.EncodeToString(sum[:8])
			if fp == lastImgFP {
				return // 同一张截图，避免重复发送
			}
			lastImgFP = fp
			if th := threshold(); th > 0 && int64(len(img)) > th {
				log.Printf("剪贴板截图 %s，超过自动同步阈值 %s，已跳过",
					bytesize.Human(int64(len(img))), bytesize.Human(th))
				return
			}
			log.Printf("检测到剪贴板截图 %s，开始同步", bytesize.Human(int64(len(img))))
			sender.SendImage(img)
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-events:
		}
		// 心跳打在 detect 之前：一旦 detect 卡死（剪贴板线程不再应答，
		// 即历史上那个 CloseClipboard 泄漏类故障），心跳随即陈旧，
		// 看门狗会转储堆栈定位问题。
		diag.Mark("monitor")
		detect()
	}
}

func thresholdLabel(threshold int64) string {
	if threshold <= 0 {
		return "不限制"
	}
	return bytesize.Human(threshold)
}

// fingerprint 生成这批文件的指纹（路径+大小+mtime），并统计总大小（目录递归）。
func fingerprint(paths []string) (string, int64, error) {
	var sb strings.Builder
	var total int64
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			return "", 0, fmt.Errorf("stat %s: %w", p, err)
		}
		if fi.IsDir() {
			sz, err := dirSize(p)
			if err != nil {
				return "", 0, err
			}
			total += sz
		} else {
			total += fi.Size()
		}
		fmt.Fprintf(&sb, "%s|%d|%d\n", p, fi.Size(), fi.ModTime().UnixNano())
	}
	return sb.String(), total, nil
}

func dirSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				total += fi.Size()
			}
		}
		return nil
	})
	return total, err
}
