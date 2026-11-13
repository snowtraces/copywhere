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

// favoritesFilePath 返回消息收藏记录文件（与配置文件同目录，默认 ~/.copywhere/favorites.json）。
func favoritesFilePath(cfgPath string) string {
	dir := filepath.Dir(cfgPath)
	if cfgPath == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".copywhere")
	}
	return filepath.Join(dir, "favorites.json")
}

// feedRecordKey 生成单条记录的去重键。优先使用独立 ID，为空时回退至内容字段组合。
func feedRecordKey(r panel.FileView) string {
	if r.ID != "" {
		return r.ID
	}
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
		panel.EnsureRecordID(&r)
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
	for i := range list {
		panel.EnsureRecordID(&list[i])
	}
	return sanitizeFeed(list)
}

// saveFeed 把传输记录落盘写入指定路径（feed.json）。
func saveFeed(path string, list []panel.FileView) error {
	clean := sanitizeFeed(list)
	if clean == nil {
		clean = []panel.FileView{}
	}
	for i := range clean {
		panel.EnsureRecordID(&clean[i])
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

// loadFavorites 从指定路径读取独立收藏记录文件（favorites.json）；文件损坏或不存在时返回空切片。
func loadFavorites(path string) []panel.FileView {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var list []panel.FileView
	if err := json.Unmarshal(data, &list); err != nil {
		log.Printf("读取消息收藏 %s 失败（可能已损坏）: %v", path, err)
		return nil
	}
	for i := range list {
		panel.EnsureRecordID(&list[i])
		list[i].Fav = true
		runes := []rune(list[i].Detail)
		if len(runes) > maxFeedDetailLen {
			list[i].Detail = string(runes[:maxFeedDetailLen])
		}
	}
	return list
}

// saveFavorites 把收藏记录列表落盘写入独立收藏文件（favorites.json）。
func saveFavorites(path string, list []panel.FileView) error {
	clean := make([]panel.FileView, 0, len(list))
	for _, r := range list {
		panel.EnsureRecordID(&r)
		r.Fav = true
		runes := []rune(r.Detail)
		if len(runes) > maxFeedDetailLen {
			r.Detail = string(runes[:maxFeedDetailLen])
		}
		clean = append(clean, r)
	}
	data, err := json.MarshalIndent(clean, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化消息收藏失败: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("创建消息收藏目录失败: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("写入消息收藏失败: %w", err)
	}
	return nil
}

