package wailsui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"copywhere/internal/config"
	"copywhere/internal/panel"
	"copywhere/internal/record"
)

func TestFeedFilePath(t *testing.T) {
	custom := filepath.Join(t.TempDir(), "config.json")
	got := feedFilePath(custom)
	want := filepath.Join(filepath.Dir(custom), "feed.json")
	if got != want {
		t.Fatalf("feedFilePath(%q) = %q, want %q", custom, got, want)
	}

	def := feedFilePath("")
	home, _ := os.UserHomeDir()
	expectedDef := filepath.Join(home, ".copywhere", "feed.json")
	if def != expectedDef {
		t.Fatalf("feedFilePath(\"\") = %q, want %q", def, expectedDef)
	}
}

func TestFeedSaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "feed.json")

	// 文件尚不存在时返回 nil
	if list := loadFeed(p); list != nil {
		t.Fatalf("loadFeed 尚未存在的文件应返回 nil，实际返回 %v", list)
	}

	input := []panel.FileView{
		{
			Time:   "10:00:00",
			Date:   "2026-11-13",
			In:     true,
			Peer:   "Alice",
			Name:   "file1.txt",
			Size:   100,
			Status: "成功",
			Detail: "接收成功",
			Path:   `C:\tmp\file1.txt`,
			Text:   false,
		},
		{
			Time:   "10:01:00",
			Date:   "2026-11-13",
			In:     false,
			Peer:   "Bob",
			Name:   "hello",
			Size:   5,
			Status: "成功",
			Detail: "hello",
			Text:   true,
		},
	}

	if err := saveFeed(p, input); err != nil {
		t.Fatalf("saveFeed: %v", err)
	}

	// 确认写入的文件是合法的 JSON 数组
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("os.ReadFile: %v", err)
	}
	var check []panel.FileView
	if err := json.Unmarshal(raw, &check); err != nil {
		t.Fatalf("JSON 反序列化失败: %v", err)
	}
	if len(check) != 2 {
		t.Fatalf("文件记录数 = %d，期望 2", len(check))
	}

	// loadFeed 读回
	loaded := loadFeed(p)
	if len(loaded) != 2 {
		t.Fatalf("loaded 长度 = %d, 期望 2", len(loaded))
	}
	if loaded[0].Name != "file1.txt" || !loaded[0].In || loaded[0].Text {
		t.Fatalf("loaded[0] 不匹配: %+v", loaded[0])
	}
	if loaded[1].Name != "hello" || loaded[1].In || !loaded[1].Text {
		t.Fatalf("loaded[1] 不匹配: %+v", loaded[1])
	}
}

func TestFeedCorruptFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "feed.json")
	if err := os.WriteFile(p, []byte("invalid json content{{{"), 0o600); err != nil {
		t.Fatalf("写坏文件: %v", err)
	}

	// 读取损坏文件不应崩溃，返回 nil
	list := loadFeed(p)
	if list != nil {
		t.Fatalf("损坏文件应返回 nil，实际返回 %v", list)
	}
}

func TestFeedSanitize(t *testing.T) {
	// 超长条数测试（超过 400 条）
	total := maxFeedRecords + 50
	items := make([]panel.FileView, total)
	for i := 0; i < total; i++ {
		items[i] = panel.FileView{
			Name: string(rune('a' + (i % 26))),
			Size: int64(i),
		}
	}
	cleaned := sanitizeFeed(items)
	if len(cleaned) != maxFeedRecords {
		t.Fatalf("sanitizeFeed 后长度 = %d，期望 %d", len(cleaned), maxFeedRecords)
	}
	// 保留的是最近的记录（末尾的）
	if cleaned[0].Size != 50 || cleaned[len(cleaned)-1].Size != int64(total-1) {
		t.Fatalf("保留的记录窗口不正确")
	}

	// 超长 Detail 测试（超过 2000 字符）
	longDetail := strings.Repeat("字", maxFeedDetailLen+100)
	single := []panel.FileView{{
		Name:   "long.txt",
		Detail: longDetail,
	}}
	cleanedSingle := sanitizeFeed(single)
	if runes := []rune(cleanedSingle[0].Detail); len(runes) != maxFeedDetailLen {
		t.Fatalf("Detail 字符数 = %d，期望裁剪到 %d", len(runes), maxFeedDetailLen)
	}
}

func TestServiceRecordsAndFeedOperations(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("config.Default: %v", err)
	}
	cfg.Path = filepath.Join(dir, "config.json")
	feedFile := filepath.Join(dir, "feed.json")

	// 预先写入一条历史数据
	initial := []panel.FileView{{
		Time: "09:00:00", Date: "2026-11-13", Name: "hist.txt", Size: 10,
	}}
	if err := saveFeed(feedFile, initial); err != nil {
		t.Fatalf("saveFeed: %v", err)
	}

	core := newFakeCore()
	bus := panel.NewBus()
	// 在总线中放入一条启动前收到的文件记录
	bus.AddFile(record.File{
		Time: time.Now(), Name: "live.txt", Size: 20,
	})

	svc := NewService(core, cfg, cfg.Path, bus)

	// Records 应包含历史记录 + 总线记录（共 2 条）
	recs := svc.Records()
	if len(recs) != 2 {
		t.Fatalf("Records() 返回条数 = %d，期望 2（含预置与总线记录）", len(recs))
	}
	if recs[0].Name != "hist.txt" || recs[1].Name != "live.txt" {
		t.Fatalf("Records() 内容不符合预期: %+v", recs)
	}

	// 追加一条新记录
	svc.appendFeedRecord(panel.FileView{
		Time: "11:00:00", Date: "2026-11-13", Name: "new.txt", Size: 30,
	})
	if len(svc.Records()) != 3 {
		t.Fatalf("appendFeedRecord 后条数 = %d，期望 3", len(svc.Records()))
	}
	// 重复追加同一条记录应去重
	svc.appendFeedRecord(panel.FileView{
		Time: "11:00:00", Date: "2026-11-13", Name: "new.txt", Size: 30,
	})
	if len(svc.Records()) != 3 {
		t.Fatalf("重复追加应去重，实际条数 = %d", len(svc.Records()))
	}

	// 立即落盘并验证磁盘文件
	svc.FlushFeed()
	onDisk := loadFeed(feedFile)
	if len(onDisk) != 3 {
		t.Fatalf("FlushFeed 落盘后条数 = %d，期望 3", len(onDisk))
	}

	// SaveFeed 覆写测试
	customList := []panel.FileView{
		{Name: "custom1.txt", Size: 1},
		{Name: "custom2.txt", Size: 2},
	}
	if err := svc.SaveFeed(customList); err != nil {
		t.Fatalf("SaveFeed: %v", err)
	}
	if len(svc.Records()) != 2 {
		t.Fatalf("SaveFeed 后 Records() 条数 = %d，期望 2", len(svc.Records()))
	}
	onDisk2 := loadFeed(feedFile)
	if len(onDisk2) != 2 || onDisk2[0].Name != "custom1.txt" {
		t.Fatalf("磁盘文件被 SaveFeed 覆写验证失败: %+v", onDisk2)
	}

	// ClearFeed 清空测试
	if err := svc.ClearFeed(); err != nil {
		t.Fatalf("ClearFeed: %v", err)
	}
	if len(svc.Records()) != 0 {
		t.Fatalf("ClearFeed 后 Records() 应为空，实际 %d", len(svc.Records()))
	}
	if len(bus.Files()) != 0 {
		t.Fatalf("ClearFeed 后 bus.Files() 应已被清空，实际 %d", len(bus.Files()))
	}
	onDiskEmpty := loadFeed(feedFile)
	if len(onDiskEmpty) != 0 {
		t.Fatalf("ClearFeed 后磁盘应为空记录，实际 %d", len(onDiskEmpty))
	}
}

