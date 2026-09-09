// Package monitor 轮询本机剪贴板，检测到新的文件/文本后触发同步。
//
// 防回环设计：
//  1. 本进程写剪贴板后记录新的 sequence number（OwnSeq），轮询时跳过；
//  2. 刚从远端接收内容后的 3 秒内忽略剪贴板变化（LastReceivedAt 兜底竞态窗口）。
package monitor

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"copywhere/internal/bytesize"
	"copywhere/internal/clip"
)

// Guard 提供防回环状态。
type Guard struct {
	OwnSeq         *atomic.Uint32 // 本进程最近一次写剪贴板后的序号
	LastReceivedAt *atomic.Int64  // 最近一次收到远端内容的 unix nano
}

// Sender 是监控器触发的发送接口（由 app 实现）。
type Sender interface {
	SendFiles(paths []string, total int64)
	SendText(text string)
}

const pollInterval = 400 * time.Millisecond

// Run 阻塞运行剪贴板监控，直到 ctx 结束。
func Run(ctx context.Context, guard Guard, threshold int64, textSync bool, sender Sender) {
	lastSeq := clip.Seq() // 忽略启动时剪贴板里的既有内容
	var lastFP, lastText string

	log.Printf("剪贴板监控已启动（每 %s 轮询，自动同步阈值 %s）",
		pollInterval, thresholdLabel(threshold))
	t := time.NewTicker(pollInterval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		seq := clip.Seq()
		if seq == lastSeq {
			continue
		}
		lastSeq = seq

		if guard.OwnSeq != nil && seq == guard.OwnSeq.Load() {
			continue // 本进程自己写入的
		}
		if guard.LastReceivedAt != nil &&
			time.Since(time.Unix(0, guard.LastReceivedAt.Load())) < 3*time.Second {
			continue // 刚收到远端内容，防回环兜底
		}

		// 优先处理文件列表
		if files, err := clip.ReadFiles(); err == nil && len(files) > 0 {
			fp, total, ferr := fingerprint(files)
			if ferr != nil {
				log.Printf("读取剪贴板文件信息失败: %v", ferr)
				continue
			}
			if fp == lastFP {
				continue // 同一批文件，避免重复发送
			}
			lastFP = fp
			if threshold > 0 && total > threshold {
				log.Printf("剪贴板文件共 %s，超过自动同步阈值 %s，已跳过（可用 send 命令手动发送）",
					bytesize.Human(total), bytesize.Human(threshold))
				continue
			}
			log.Printf("检测到剪贴板文件 %d 个，共 %s，开始同步", len(files), bytesize.Human(total))
			sender.SendFiles(files, total)
			continue
		}

		// 再处理文本
		if !textSync {
			continue
		}
		if text, err := clip.ReadText(); err == nil && text != "" {
			if text == lastText {
				continue
			}
			lastText = text
			log.Printf("检测到剪贴板文本 %d 字符，开始同步", len(text))
			sender.SendText(text)
		}
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
