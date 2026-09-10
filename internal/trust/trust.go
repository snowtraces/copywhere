// Package trust 维护配对节点的独立令牌列表（peers.json），与 config.json 的
// 共享 token 完全分离：config token 不被配对读写，仅作为旧版兼容回退。
//
// 每个已配对节点两条令牌：
//   - PairToken：本节点为该对端签发的令牌，对端发来内容时应携带它；
//   - PeerToken：对端为本节点签发的令牌，本节点向对端发送时应携带它。
//
// 单独吊销（解除配对）即从列表删除，立即双向失效，无需改动 config token。
package trust

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Entry 是一个已配对节点的记录。
type Entry struct {
	Name      string    `json:"name"`       // 配对时的节点名（仅展示用）
	PairToken string    `json:"pair_token"` // 本节点签发给该对端的令牌
	PeerToken string    `json:"peer_token"` // 该对端签发给本节点的令牌
	PairedAt  time.Time `json:"paired_at"`
}

// Paired 是带节点指纹的列表视图。
type Paired struct {
	ID string `json:"id"`
	Entry
}

// Store 是线程安全的配对令牌列表，持久化为单个 JSON 文件。
type Store struct {
	mu      sync.Mutex
	path    string
	entries map[string]Entry
}

// New 返回一个空的信任库（不读盘），用于 Load 失败时的内存兜底。
func New(path string) *Store {
	return &Store{path: path, entries: map[string]Entry{}}
}

// Load 读取信任库；文件不存在时返回空库。
func Load(path string) (*Store, error) {
	s := &Store{path: path, entries: map[string]Entry{}}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &s.entries); err != nil {
		return nil, err
	}
	return s, nil
}

// Get 返回指定节点的配对记录。
func (s *Store) Get(id string) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[id]
	return e, ok
}

// Has 判断节点是否已配对。
func (s *Store) Has(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.entries[id]
	return ok
}

// Add 新增/覆盖一条配对记录并立即落盘。
func (s *Store) Add(id, name, pairToken, peerToken string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[id] = Entry{
		Name: name, PairToken: pairToken, PeerToken: peerToken,
		PairedAt: time.Now(),
	}
	return s.save()
}

// Remove 删除一条配对记录（吊销）并立即落盘。
func (s *Store) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.entries[id]; !ok {
		return nil
	}
	delete(s.entries, id)
	return s.save()
}

// List 返回全部配对记录（按 ID 排序）。
func (s *Store) List() []Paired {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Paired, 0, len(s.entries))
	for id, e := range s.entries {
		out = append(out, Paired{ID: id, Entry: e})
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].ID < out[j-1].ID; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// save 原子写盘（临时文件 + 改名），调用方须持有 s.mu。
func (s *Store) save() error {
	data, err := json.MarshalIndent(s.entries, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
