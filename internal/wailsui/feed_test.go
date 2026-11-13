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

	// sanitizeFeed 应为每条记录派生 ID
	if cleanedSingle[0].ID == "" {
		t.Fatal("sanitizeFeed 后记录缺少 ID")
	}
}

func TestRecordIDStability(t *testing.T) {
	// 相同属性的记录应派生出相同 ID
	r1 := panel.FileView{
		Date: "2026-11-13", Time: "10:00:00", In: true, Peer: "Alice",
		Name: "file.txt", Size: 100, Text: false, Detail: "hello",
	}
	r2 := r1 // 完全相同
	panel.EnsureRecordID(&r1)
	panel.EnsureRecordID(&r2)
	if r1.ID != r2.ID {
		t.Fatalf("相同记录应派生相同 ID: %q vs %q", r1.ID, r2.ID)
	}

	// 任意字段不同，ID 应不同
	r3 := r1
	r3.Name = "other.txt"
	r3.ID = ""
	panel.EnsureRecordID(&r3)
	if r3.ID == r1.ID {
		t.Fatalf("不同记录 ID 不应相同: both = %q", r1.ID)
	}

	// 已有 ID 的记录不会被覆盖
	r4 := panel.FileView{ID: "my-custom-id", Name: "x.txt"}
	panel.EnsureRecordID(&r4)
	if r4.ID != "my-custom-id" {
		t.Fatalf("已有 ID 被意外修改: %q", r4.ID)
	}
}

func TestFavoritesFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	favFile := filepath.Join(dir, "favorites.json")

	favs := []panel.FileView{
		{ID: "msg_aaa", Date: "2026-11-13", Time: "10:00:00", Name: "note.txt", Size: 5, Text: true, Detail: "hi"},
		{ID: "msg_bbb", Date: "2026-11-13", Time: "11:00:00", Name: "image.png", Size: 2048},
	}
	if err := saveFavorites(favFile, favs); err != nil {
		t.Fatalf("saveFavorites: %v", err)
	}

	loaded := loadFavorites(favFile)
	if len(loaded) != 2 {
		t.Fatalf("loadFavorites 返回条数 = %d，期望 2", len(loaded))
	}
	// 所有加载的收藏条目 Fav 字段必须为 true，ID 必须保持
	for _, r := range loaded {
		if !r.Fav {
			t.Errorf("loadFavorites: 条目 %q Fav 应为 true", r.Name)
		}
		if r.ID == "" {
			t.Errorf("loadFavorites: 条目 %q 缺少 ID", r.Name)
		}
	}
	if loaded[0].ID != "msg_aaa" || loaded[1].ID != "msg_bbb" {
		t.Fatalf("ID 未正确保留: %v", []string{loaded[0].ID, loaded[1].ID})
	}
}

func TestSaveFavoritesAndFeedSync(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("config.Default: %v", err)
	}
	cfg.Path = filepath.Join(dir, "config.json")

	core := newFakeCore()
	bus := panel.NewBus()
	svc := NewService(core, cfg, cfg.Path, bus)

	// 追加两条 feed 记录
	r1 := panel.FileView{Date: "2026-11-13", Time: "09:00:00", Name: "a.txt", Size: 1}
	r2 := panel.FileView{Date: "2026-11-13", Time: "09:01:00", Name: "b.txt", Size: 2}
	panel.EnsureRecordID(&r1)
	panel.EnsureRecordID(&r2)
	svc.appendFeedRecord(r1)
	svc.appendFeedRecord(r2)

	// 收藏 r1
	favToSave := []panel.FileView{r1}
	favToSave[0].Fav = true
	if err := svc.SaveFavorites(favToSave); err != nil {
		t.Fatalf("SaveFavorites: %v", err)
	}

	// feed 中 r1 应被打上 Fav=true，r2 保持 false
	recs := svc.Records()
	if len(recs) != 2 {
		t.Fatalf("Records() 返回条数 = %d，期望 2", len(recs))
	}
	for _, r := range recs {
		if r.Name == "a.txt" && !r.Fav {
			t.Errorf("feed 中 a.txt Fav 应为 true，实际 false")
		}
		if r.Name == "b.txt" && r.Fav {
			t.Errorf("feed 中 b.txt Fav 应为 false，实际 true")
		}
	}

	// favorites.json 应独立落盘，仅含 r1
	favFile := filepath.Join(dir, "favorites.json")
	onDiskFavs := loadFavorites(favFile)
	if len(onDiskFavs) != 1 || onDiskFavs[0].ID != r1.ID {
		t.Fatalf("favorites.json 内容不符合预期: %+v", onDiskFavs)
	}

	// feed.json 中不应包含收藏内容（fav 标记只在内存中用于展示，落盘到 feed.json 仅标记）
	svc.FlushFeed()
	onDiskFeed := loadFeed(filepath.Join(dir, "feed.json"))
	if len(onDiskFeed) != 2 {
		t.Fatalf("feed.json 条数 = %d，期望 2", len(onDiskFeed))
	}

	// Favorites() 返回正确
	favs := svc.Favorites()
	if len(favs) != 1 || favs[0].ID != r1.ID {
		t.Fatalf("Favorites() 返回不符: %+v", favs)
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
