package wailsui

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"copywhere/internal/panel"
)

const (
	maxFeedRecords   = 400
	maxFeedDetailLen = 2000
	feedSaveDelay    = 400 * time.Millisecond
)

// feedFilePath 返回传输动态记录文件（与配置文件同目录，默认 ~/.copywhere/feed.json）。
func feedFilePath(cfgPath string) string {
	dir := filepath.Dir(cfgPath)
	if cfgPath == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".copywhere")
	}
	return filepath.Join(dir, "feed.json")
}

// feedRecordKey 生成单条记录的去重键。
func feedRecordKey(r panel.FileView) string {
	in := "0"
	if r.In {
		in = "1"
	}
	txt := "0"
	if r.Text {
		txt = "1"
	}
	return r.Date + "|" + r.Time + "|" + in + "|" + r.Peer + "|" + r.Name + "|" + fmt.Sprint(r.Size) + "|" + txt + "|" + r.Detail
}

// sanitizeFeed 截断保留最近 maxFeedRecords 条，并将超长 Detail 裁剪至 maxFeedDetailLen。
func sanitizeFeed(list []panel.FileView) []panel.FileView {
	if len(list) > maxFeedRecords {
		list = list[len(list)-maxFeedRecords:]
	}
	out := make([]panel.FileView, len(list))
	for i, r := range list {
		runes := []rune(r.Detail)
		if len(runes) > maxFeedDetailLen {
			r.Detail = string(runes[:maxFeedDetailLen])
		}
		out[i] = r
	}
	return out
}

// loadFeed 从指定路径读取传输记录；文件损坏或不存在时返回空切片。
func loadFeed(path string) []panel.FileView {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var list []panel.FileView
	if err := json.Unmarshal(data, &list); err != nil {
		log.Printf("读取传输动态 %s 失败（可能已损坏）: %v", path, err)
		return nil
	}
	return sanitizeFeed(list)
}

// saveFeed 把传输记录落盘写入指定路径。
func saveFeed(path string, list []panel.FileView) error {
	clean := sanitizeFeed(list)
	if clean == nil {
		clean = []panel.FileView{}
	}
	data, err := json.MarshalIndent(clean, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化传输动态失败: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("创建传输动态目录失败: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("写入传输动态失败: %w", err)
	}
	return nil
}

