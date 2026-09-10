package trust

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMissingCreatesEmpty(t *testing.T) {
	s, err := Load(filepath.Join(t.TempDir(), "peers.json"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Has("x") {
		t.Fatal("空库不应包含任何记录")
	}
}

func TestAddGetListRemove(t *testing.T) {
	path := filepath.Join(t.TempDir(), "peers.json")
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add("id-b", "节点B", "ptok-b", "rtok-b"); err != nil {
		t.Fatal(err)
	}
	if err := s.Add("id-a", "节点A", "ptok-a", "rtok-a"); err != nil {
		t.Fatal(err)
	}
	e, ok := s.Get("id-a")
	if !ok || e.Name != "节点A" || e.PairToken != "ptok-a" || e.PeerToken != "rtok-a" {
		t.Fatalf("Get 结果不符: %+v ok=%v", e, ok)
	}
	list := s.List()
	if len(list) != 2 || list[0].ID != "id-a" || list[1].ID != "id-b" {
		t.Fatalf("List 应按 ID 排序: %+v", list)
	}
	// 落盘验证：重新加载应读到相同内容
	s2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	e2, ok := s2.Get("id-b")
	if !ok || e2.PeerToken != "rtok-b" {
		t.Fatalf("重新加载后记录丢失: %+v", e2)
	}
	if err := s2.Remove("id-b"); err != nil {
		t.Fatal(err)
	}
	if s2.Has("id-b") {
		t.Fatal("Remove 后记录应消失")
	}
	s3, _ := Load(path)
	if s3.Has("id-b") {
		t.Fatal("Remove 应已落盘")
	}
	// 未存在的 Remove 应为 no-op
	if err := s3.Remove("ghost"); err != nil {
		t.Fatalf("Remove 不存在的记录不应报错: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("信任库文件应存在: %v", err)
	}
}

func TestAddOverwrites(t *testing.T) {
	s, _ := Load(filepath.Join(t.TempDir(), "peers.json"))
	s.Add("id", "A", "p1", "r1")
	s.Add("id", "A", "p2", "r2")
	e, _ := s.Get("id")
	if e.PairToken != "p2" || e.PeerToken != "r2" {
		t.Fatalf("重复 Add 应覆盖: %+v", e)
	}
	if len(s.List()) != 1 {
		t.Fatalf("重复 Add 不应产生重复条目")
	}
}
