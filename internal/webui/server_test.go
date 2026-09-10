package webui

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"copywhere/internal/config"
	"copywhere/internal/discovery"
	"copywhere/internal/trust"
)

// fakeCore 是测试用的面板后端桩。
type fakeCore struct {
	paused bool
	peers  []discovery.Peer
	sent   []string
	files  [][]string
}

func (f *fakeCore) Peers() []discovery.Peer { return f.peers }
func (f *fakeCore) SelfName() string        { return "test-node" }
func (f *fakeCore) SelfIP() string          { return "10.0.0.1" }
func (f *fakeCore) SetPaused(p bool)        { f.paused = p }
func (f *fakeCore) IsPaused() bool          { return f.paused }
func (f *fakeCore) SendText(text string)    { f.sent = append(f.sent, text) }
func (f *fakeCore) SendFiles(paths []string, total int64) {
	f.files = append(f.files, paths)
}
func (f *fakeCore) PairedWith(id string) bool     { return false }
func (f *fakeCore) RejectedWith(id string) bool   { return false }
func (f *fakeCore) PairedList() []trust.Paired    { return nil }
func (f *fakeCore) PairWith(id string) error      { return nil }
func (f *fakeCore) RespondPair(accept bool) error { return nil }
func (f *fakeCore) PendingPair() (string, string, bool) {
	return "", "", false
}
func (f *fakeCore) Unpair(id string) error { return nil }
func (f *fakeCore) SyncKVM()               {}

func newTestServer(t *testing.T) (*Server, *fakeCore) {
	t.Helper()
	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	cfg.WebPort = 0 // 随机端口，避免测试间冲突
	core := &fakeCore{}
	srv, err := NewServer(core, cfg, filepath.Join(t.TempDir(), "config.json"), NewBus())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Shutdown)
	return srv, core
}

// splitURL 把 srv.URL() 拆成不带查询参数的 base（以 / 结尾）与访问 token。
func splitURL(srv *Server) (base, key string) {
	i := strings.Index(srv.URL(), "?")
	return srv.URL()[:i], srv.URL()[i+5:] // ".../?key=TOKEN" → "http://host/" 与 "TOKEN"
}

func TestAuthRequired(t *testing.T) {
	srv, _ := newTestServer(t)
	base, _ := splitURL(srv)
	resp, err := http.Get(base) // 不带 key
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无令牌应 401，实际 %d", resp.StatusCode)
	}
}

func TestAuthViaQueryThenCookie(t *testing.T) {
	srv, _ := newTestServer(t)
	base, _ := splitURL(srv)
	client := &http.Client{}
	resp, err := client.Get(srv.URL()) // URL 带 key
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("带 key 应 200，实际 %d", resp.StatusCode)
	}
	var hasCookie bool
	for _, c := range resp.Cookies() {
		if c.Name == "cwkey" && c.Value != "" {
			hasCookie = true
		}
	}
	if !hasCookie {
		t.Fatal("首次带 key 访问应种下 cwkey cookie")
	}
	// 随后用 cookie 访问任意 API，不再需要 key 参数
	req, _ := http.NewRequest("GET", base+"api/state", nil)
	for _, c := range resp.Cookies() {
		req.AddCookie(c)
	}
	resp2, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("cookie 访问 /api/state 应 200，实际 %d", resp2.StatusCode)
	}
}

func TestStateAndPause(t *testing.T) {
	srv, core := newTestServer(t)
	base, key := splitURL(srv)
	core.peers = []discovery.Peer{{ID: "x", Name: "peer-a", IPs: []string{"10.0.0.2"}, TCPPort: 47831}}

	resp, err := http.Get(base + "api/state?key=" + key)
	if err != nil {
		t.Fatal(err)
	}
	var st map[string]any
	json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	if st["node_name"] != srv.cfg.NodeName || st["peer_count"] != float64(1) {
		t.Fatalf("state 不符合预期: %v", st)
	}

	resp, err = http.Post(base+"api/pause?key="+key, "application/json",
		strings.NewReader(`{"paused":true}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !core.paused {
		t.Fatal("api/pause 应切换到暂停状态")
	}
}

func TestSendText(t *testing.T) {
	srv, core := newTestServer(t)
	base, key := splitURL(srv)
	resp, err := http.Post(base+"api/send-text?key="+key, "application/json",
		strings.NewReader(`{"text":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// 无在线节点时应 409 且不发送
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("无节点发送文本应 409，实际 %d", resp.StatusCode)
	}
	core.peers = []discovery.Peer{{ID: "x", Name: "p", IPs: []string{"1.2.3.4"}, TCPPort: 1}}
	resp, err = http.Post(base+"api/send-text?key="+key, "application/json",
		strings.NewReader(`{"text":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("有节点发送文本应 200，实际 %d", resp.StatusCode)
	}
	// SendText 在 goroutine 中执行
	deadline := time.Now().Add(2 * time.Second)
	for len(core.sent) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(core.sent) != 1 || core.sent[0] != "hello" {
		t.Fatalf("文本应被转发给 Core，实际 %v", core.sent)
	}
}

func TestConfigSave(t *testing.T) {
	srv, _ := newTestServer(t)
	base, key := splitURL(srv)
	resp, err := http.Post(base+"api/config?key="+key, "application/json",
		strings.NewReader(`{"max_auto_copy_mb": 5, "auto_paste": false, "node_name": "renamed"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	if out["ok"] != true {
		t.Fatalf("保存配置应成功: %v", out)
	}
	if rr, _ := out["restart_required"].([]any); len(rr) != 1 || rr[0] != "node_name" {
		t.Fatalf("node_name 应标记为需重启: %v", out)
	}
	// 内存配置生效验证（save 落盘到 t.TempDir）
	if srv.cfg.MaxAutoCopyMB != 5 || srv.cfg.AutoPaste || srv.cfg.NodeName != "renamed" {
		t.Fatalf("配置未生效: %+v", srv.cfg)
	}
}

func TestKVMConfigSave(t *testing.T) {
	srv, _ := newTestServer(t)
	base, key := splitURL(srv)
	resp, err := http.Post(base+"api/config?key="+key, "application/json",
		strings.NewReader(`{"kvm_left":"LeftPC","kvm_right":"","kvm_enabled":false}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	if out["ok"] != true {
		t.Fatalf("保存 KVM 配置应成功: %v", out)
	}
	rr, _ := out["restart_required"].([]any)
	got := map[string]bool{}
	for _, v := range rr {
		got[v.(string)] = true
	}
	// 左右邻居已改为热更新（不重启生效），只有开关需要重启
	if got["kvm_left"] || got["kvm_right"] {
		t.Fatalf("邻居配置不应标记需重启: %v", out)
	}
	if !got["kvm_enabled"] {
		t.Fatalf("kvm_enabled 应标记需重启: %v", out)
	}
	if srv.cfg.KVMLeft != "LeftPC" || srv.cfg.KVMRight != "" {
		t.Fatalf("KVM 邻居未生效: %+v", srv.cfg)
	}
	if srv.cfg.KVMEnabled == nil || *srv.cfg.KVMEnabled {
		t.Fatalf("kvm_enabled 未生效: %+v", srv.cfg.KVMEnabled)
	}
}

func TestSSEStream(t *testing.T) {
	srv, _ := newTestServer(t)
	base, key := splitURL(srv)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", base+"api/events?key="+key, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("SSE Content-Type 不符: %q", ct)
	}
	// 建连后应立即收到 peers、state 两个事件
	sc := bufio.NewScanner(resp.Body)
	got := 0
	for sc.Scan() && got < 2 {
		line := sc.Text()
		if strings.HasPrefix(line, "data: ") {
			var ev Event
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
				t.Fatalf("事件 JSON 非法: %v", err)
			}
			if ev.Type != EvPeers && ev.Type != EvState {
				t.Fatalf("首推事件类型意外: %s", ev.Type)
			}
			got++
		}
	}
	if got < 2 {
		t.Fatalf("SSE 建连应首推 2 个快照事件，实际 %d", got)
	}
}

func TestPairEndpoints(t *testing.T) {
	srv, _ := newTestServer(t)
	base, key := splitURL(srv)

	// pending：无待裁决请求
	resp, err := http.Get(base + "api/pair/pending?key=" + key)
	if err != nil {
		t.Fatal(err)
	}
	var pend map[string]any
	json.NewDecoder(resp.Body).Decode(&pend)
	resp.Body.Close()
	if v, ok := pend["pending"]; !ok || v != nil {
		t.Fatalf("pending 应为 null: %v", pend)
	}

	// paired：空列表（fakeCore 返回 nil → 序列化为 []）
	resp, err = http.Get(base + "api/paired?key=" + key)
	if err != nil {
		t.Fatal(err)
	}
	var pl map[string]any
	json.NewDecoder(resp.Body).Decode(&pl)
	resp.Body.Close()
	if _, ok := pl["paired"]; !ok {
		t.Fatalf("paired 字段缺失: %v", pl)
	}

	// 缺少 id → 400
	resp, err = http.Post(base+"api/pair?key="+key, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("缺少 id 应 400，实际 %d", resp.StatusCode)
	}

	// 正常发起（fakeCore 成功返回）→ 200
	resp, err = http.Post(base+"api/pair?key="+key, "application/json",
		strings.NewReader(`{"id":"peer-x"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("发起配对应 200，实际 %d", resp.StatusCode)
	}
}

func TestIndexServed(t *testing.T) {
	srv, _ := newTestServer(t)
	resp, err := http.Get(srv.URL()) // 自带 key
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "copywhere") {
		t.Fatal("面板页面应包含 copywhere 标识")
	}
}

func TestFaviconsServed(t *testing.T) {
	srv, _ := newTestServer(t)
	// 测试未带任何 key 或 cookie 时拉取各种 favicon
	base, _ := splitURL(srv)

	tests := []struct {
		path        string
		contentType string
	}{
		{"favicon.ico", "image/x-icon"},
		{"favicon.svg", "image/svg+xml"},
		{"favicon.png", "image/png"},
	}

	for _, tc := range tests {
		resp, err := http.Get(base + tc.path)
		if err != nil {
			t.Fatalf("%s 请求失败: %v", tc.path, err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("%s 读取失败: %v", tc.path, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s 应返回 200，实际 %d", tc.path, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); ct != tc.contentType {
			t.Fatalf("%s Content-Type 应为 %q，实际 %q", tc.path, tc.contentType, ct)
		}
		if len(body) == 0 {
			t.Fatalf("%s 内容不应为空", tc.path)
		}
	}
}

