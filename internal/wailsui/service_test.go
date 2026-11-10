package wailsui

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"copywhere/internal/config"
	"copywhere/internal/discovery"
	"copywhere/internal/trust"
	"copywhere/internal/webui"
)

// fakeCore 是 Service 依赖的最小核心实现，用于在不启动真实服务
// （端口/剪贴板/KVM）的情况下测试视图与操作。
type fakeCore struct {
	mu sync.Mutex

	peers     []discovery.Peer
	paired    map[string]bool
	rejected  map[string]bool
	pairedLst []trust.Paired
	paused    bool

	sentFiles  [][]string
	sentTexts  []string
	copied     []string
	kvmEnabled []bool
	touchOn    []bool
	touchSpeed []int
	syncCalls  int

	pendingID   string
	pendingName string
	pendingOK   bool

	sendFilesCh chan []string
}

func newFakeCore() *fakeCore {
	return &fakeCore{
		paired:      map[string]bool{},
		rejected:    map[string]bool{},
		sendFilesCh: make(chan []string, 8),
	}
}

func (f *fakeCore) Peers() []discovery.Peer {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]discovery.Peer, len(f.peers))
	copy(out, f.peers)
	return out
}
func (f *fakeCore) SelfName() string { return "self-node" }
func (f *fakeCore) SelfIP() string   { return "10.0.0.1" }
func (f *fakeCore) SetPaused(p bool) {
	f.mu.Lock()
	f.paused = p
	f.mu.Unlock()
}
func (f *fakeCore) IsPaused() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.paused
}
func (f *fakeCore) SendFiles(paths []string, total int64) {
	f.mu.Lock()
	f.sentFiles = append(f.sentFiles, append([]string(nil), paths...))
	f.mu.Unlock()
	select {
	case f.sendFilesCh <- paths:
	default:
	}
}
func (f *fakeCore) SendText(text string) {
	f.mu.Lock()
	f.sentTexts = append(f.sentTexts, text)
	f.mu.Unlock()
}
func (f *fakeCore) PairedWith(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.paired[id]
}
func (f *fakeCore) RejectedWith(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rejected[id]
}
func (f *fakeCore) PairedList() []trust.Paired {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]trust.Paired, len(f.pairedLst))
	copy(out, f.pairedLst)
	return out
}
func (f *fakeCore) PairWith(id string) error {
	f.mu.Lock()
	f.paired[id] = true
	f.mu.Unlock()
	return nil
}
func (f *fakeCore) RespondPair(accept bool) error {
	f.mu.Lock()
	f.pendingOK = false
	f.mu.Unlock()
	return nil
}
func (f *fakeCore) PendingPair() (string, string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pendingID, f.pendingName, f.pendingOK
}
func (f *fakeCore) Unpair(id string) error {
	f.mu.Lock()
	delete(f.paired, id)
	f.mu.Unlock()
	return nil
}
func (f *fakeCore) SyncKVMFrom(oldLeft, oldRight string) {
	f.mu.Lock()
	f.syncCalls++
	f.mu.Unlock()
}
func (f *fakeCore) SetKVMEnabled(on bool) {
	f.mu.Lock()
	f.kvmEnabled = append(f.kvmEnabled, on)
	f.mu.Unlock()
}
func (f *fakeCore) SetTouchpadGestures(on bool) {
	f.mu.Lock()
	f.touchOn = append(f.touchOn, on)
	f.mu.Unlock()
}
func (f *fakeCore) SetTouchpadSpeed(pct int) {
	f.mu.Lock()
	f.touchSpeed = append(f.touchSpeed, pct)
	f.mu.Unlock()
}
func (f *fakeCore) WatchMode() string { return "listener" }
func (f *fakeCore) CopyText(text string) error {
	f.mu.Lock()
	f.copied = append(f.copied, text)
	f.mu.Unlock()
	return nil
}

func (f *fakeCore) lastKVMEnabled() (bool, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.kvmEnabled) == 0 {
		return false, false
	}
	return f.kvmEnabled[len(f.kvmEnabled)-1], true
}

func (f *fakeCore) sentTextSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sentTexts...)
}

// newTestService 构造带临时配置/接收目录的绑定服务。
func newTestService(t *testing.T) (*Service, *fakeCore, *config.Config) {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("config.Default: %v", err)
	}
	cfg.Path = filepath.Join(dir, "config.json")
	cfg.ReceiveDir = filepath.Join(dir, "files")
	if err := os.MkdirAll(cfg.ReceiveDir, 0o755); err != nil {
		t.Fatalf("创建接收目录: %v", err)
	}
	core := newFakeCore()
	return NewService(core, cfg, cfg.Path, webui.NewBus()), core, cfg
}

func TestStateAndPeersView(t *testing.T) {
	s, core, cfg := newTestService(t)
	cfg.KVMLeft = "LEFT-PC"
	cfg.KVMRight = "RIGHT-PC"
	core.peers = []discovery.Peer{
		{ID: "p1", Name: "LEFT-PC", IPs: []string{"192.168.0.2"}, TCPPort: 47831, LastSeen: time.Now()},
		{ID: "p2", Name: "RIGHT-PC", IPs: []string{"192.168.0.3"}, TCPPort: 47831, LastSeen: time.Now()},
		{ID: "p3", Name: "OTHER", IPs: []string{"192.168.0.4"}, TCPPort: 47831},
	}
	core.paired["p1"] = true
	core.rejected["p3"] = true
	core.pairedLst = []trust.Paired{{ID: "p1", Entry: trust.Entry{Name: "LEFT-PC", PairedAt: time.Now()}}}

	st := s.State()
	if st.NodeName != cfg.NodeName {
		t.Fatalf("NodeName = %q, want %q", st.NodeName, cfg.NodeName)
	}
	if st.SelfIP != "10.0.0.1" || st.PeerCount != 3 || st.PairedCount != 1 {
		t.Fatalf("State 快照不符: %+v", st)
	}
	if !st.KVMEnabled || st.KVMLeft != "LEFT-PC" || st.KVMRight != "RIGHT-PC" {
		t.Fatalf("KVM 状态不符: %+v", st)
	}
	if st.WatchMode != "listener" || st.Version == "" {
		t.Fatalf("诊断字段不符: %+v", st)
	}

	peers := s.Peers()
	if len(peers) != 3 {
		t.Fatalf("Peers 数量 = %d, want 3", len(peers))
	}
	byName := map[string]Peer{}
	for _, p := range peers {
		byName[p.Name] = p
	}
	if byName["LEFT-PC"].Side != "left" || !byName["LEFT-PC"].Paired {
		t.Fatalf("左邻视图不符: %+v", byName["LEFT-PC"])
	}
	if byName["RIGHT-PC"].Side != "right" {
		t.Fatalf("右邻视图不符: %+v", byName["RIGHT-PC"])
	}
	if !byName["OTHER"].Blocked || byName["OTHER"].Side != "" {
		t.Fatalf("拒绝状态/布局不符: %+v", byName["OTHER"])
	}
}

func TestSendTextValidation(t *testing.T) {
	s, core, _ := newTestService(t)

	if err := s.SendText("   "); err == nil || !strings.Contains(err.Error(), "不能为空") {
		t.Fatalf("空文本应报错，实际: %v", err)
	}
	if err := s.SendText("hello"); err == nil || !strings.Contains(err.Error(), "未发现在线节点") {
		t.Fatalf("无节点应报错，实际: %v", err)
	}

	core.peers = []discovery.Peer{{ID: "p1", Name: "PEER"}}
	if err := s.SendText("hello"); err != nil {
		t.Fatalf("有节点时应接受: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for len(core.sentTextSnapshot()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := core.sentTextSnapshot(); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("文本未交到核心: %v", got)
	}
}

func TestSendFilesValidation(t *testing.T) {
	s, core, cfg := newTestService(t)

	if err := s.SendFiles(nil); err == nil || !strings.Contains(err.Error(), "没有可发送的文件") {
		t.Fatalf("空路径应报错，实际: %v", err)
	}
	if err := s.SendFiles([]string{filepath.Join(cfg.ReceiveDir, "nope.txt")}); err == nil ||
		!strings.Contains(err.Error(), "没有可发送的文件") {
		t.Fatalf("不存在的路径应报错，实际: %v", err)
	}

	file := filepath.Join(cfg.ReceiveDir, "a.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 在线但未配对：不允许投递
	core.peers = []discovery.Peer{{ID: "p1", Name: "PEER"}}
	if err := s.SendFiles([]string{file}); err == nil || !strings.Contains(err.Error(), "未配对") {
		t.Fatalf("未配对节点应拒绝投递，实际: %v", err)
	}

	core.paired["p1"] = true
	if err := s.SendFiles([]string{file}); err != nil {
		t.Fatalf("已配对节点应接受投递: %v", err)
	}
	select {
	case paths := <-core.sendFilesCh:
		if len(paths) != 1 || paths[0] != file {
			t.Fatalf("投递路径不符: %v", paths)
		}
	case <-time.After(time.Second):
		t.Fatal("文件未交到核心")
	}
}

func TestSaveSettings(t *testing.T) {
	s, core, cfg := newTestService(t)

	if err := s.SaveSettings(nil); err == nil || !strings.Contains(err.Error(), "没有需要保存") {
		t.Fatalf("空修改应报错，实际: %v", err)
	}
	if err := s.SaveSettings(map[string]any{"kvm_speed_percent": float64(9999)}); err == nil ||
		!strings.Contains(err.Error(), "0~500") {
		t.Fatalf("非法速度应报错，实际: %v", err)
	}

	err := s.SaveSettings(map[string]any{
		"node_name":             "NEW-NAME",
		"max_auto_copy_mb":      float64(5),
		"kvm_enabled":           false,
		"kvm_speed_percent":     float64(120),
		"kvm_touchpad_gestures": true,
		"kvm_touchpad_speed":    float64(150),
	})
	if err != nil {
		t.Fatalf("合法修改保存失败: %v", err)
	}
	if cfg.NodeName != "NEW-NAME" || cfg.MaxAutoCopyMB != 5 || cfg.KVMOn() {
		t.Fatalf("内存配置未更新: %+v", cfg)
	}
	if !cfg.KVMTouchpadOn() || cfg.KVMTouchpadSpeedPct() != 150 || cfg.KVMSpeedPct != 120 {
		t.Fatalf("触控板/速度配置未更新: %+v", cfg)
	}
	if v, ok := core.lastKVMEnabled(); !ok || v {
		t.Fatalf("kvm_enabled 应触发热停用，实际 %v (called=%v)", v, ok)
	}

	data, err := os.ReadFile(cfg.Path)
	if err != nil {
		t.Fatalf("读取落盘配置: %v", err)
	}
	var onDisk config.Config
	if err := json.Unmarshal(data, &onDisk); err != nil {
		t.Fatalf("解析落盘配置: %v", err)
	}
	if onDisk.NodeName != "NEW-NAME" || onDisk.MaxAutoCopyMB != 5 {
		t.Fatalf("落盘配置不符: %+v", onDisk)
	}
}

func TestSetKVMNeighbor(t *testing.T) {
	s, _, cfg := newTestService(t)
	if err := s.SetKVMNeighbor("left", "LEFT-PC"); err != nil {
		t.Fatalf("SetKVMNeighbor: %v", err)
	}
	if cfg.KVMLeft != "LEFT-PC" {
		t.Fatalf("KVMLeft = %q", cfg.KVMLeft)
	}
	if err := s.SetKVMNeighbor("up", "x"); err == nil {
		t.Fatal("未知位置应报错")
	}
	if err := s.SetKVMNeighbor("right", ""); err != nil {
		t.Fatalf("清除右邻失败: %v", err)
	}
	if cfg.KVMRight != "" {
		t.Fatalf("KVMRight 未清除: %q", cfg.KVMRight)
	}
}

func TestPairFlow(t *testing.T) {
	s, core, _ := newTestService(t)
	if p := s.PendingPair(); p != nil {
		t.Fatalf("初始不应有待裁决请求: %+v", p)
	}
	core.pendingID, core.pendingName, core.pendingOK = "p1", "PEER", true
	p := s.PendingPair()
	if p == nil || p.ID != "p1" || p.Name != "PEER" {
		t.Fatalf("PendingPair = %+v", p)
	}
	if err := s.RespondPair(true); err != nil {
		t.Fatalf("RespondPair: %v", err)
	}
	if s.PendingPair() != nil {
		t.Fatal("裁决后不应仍有待处理请求")
	}
	if err := s.Pair("p1"); err != nil {
		t.Fatalf("Pair: %v", err)
	}
	if !core.PairedWith("p1") {
		t.Fatal("配对后 PairedWith 应为 true")
	}
	if err := s.Unpair("p1"); err != nil || core.PairedWith("p1") {
		t.Fatalf("解除配对失败: err=%v paired=%v", err, core.PairedWith("p1"))
	}
}

func TestImagePreview(t *testing.T) {
	s, _, cfg := newTestService(t)
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	inside := filepath.Join(cfg.ReceiveDir, "a.png")
	if err := os.WriteFile(inside, png, 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "b.png")
	if err := os.WriteFile(outside, png, 0o644); err != nil {
		t.Fatal(err)
	}
	txt := filepath.Join(cfg.ReceiveDir, "c.txt")
	if err := os.WriteFile(txt, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	url, err := s.ImagePreview(inside)
	if err != nil {
		t.Fatalf("预览接收目录内图片失败: %v", err)
	}
	const prefix = "data:image/png;base64,"
	if !strings.HasPrefix(url, prefix) {
		t.Fatalf("data URL 前缀不符: %q", url)
	}
	if dec, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(url, prefix)); err != nil || string(dec) != string(png) {
		t.Fatalf("图片内容不符: %v", err)
	}

	if _, err := s.ImagePreview(outside); err == nil || !strings.Contains(err.Error(), "接收目录内") {
		t.Fatalf("目录外图片应被拒绝，实际: %v", err)
	}
	if _, err := s.ImagePreview(txt); err == nil || !strings.Contains(err.Error(), "仅支持图片") {
		t.Fatalf("非图片应被拒绝，实际: %v", err)
	}
	if _, err := s.ImagePreview(filepath.Join(cfg.ReceiveDir, "missing.png")); err == nil {
		t.Fatal("不存在的图片应报错")
	}
}

func TestStartBridgeForwardsBusEvents(t *testing.T) {
	s, _, _ := newTestService(t)
	type got struct {
		name string
		ev   webui.Event
	}
	ch := make(chan got, 8)
	s.setEmitter(func(name string, data any) {
		ev, ok := data.(webui.Event)
		if !ok {
			return
		}
		select {
		case ch <- got{name: name, ev: ev}:
		default:
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.StartBridge(ctx)
	time.Sleep(20 * time.Millisecond) // 让订阅先建立

	s.bus.Log("hello world")
	deadline := time.After(2 * time.Second)
	for {
		select {
		case g := <-ch:
			if g.name != "cw:event" {
				t.Fatalf("事件名 = %q, want cw:event", g.name)
			}
			if g.ev.Type == webui.EvLog {
				if line, _ := g.ev.Data.(map[string]string)["line"]; line != "hello world" {
					t.Fatalf("日志事件内容不符: %+v", g.ev.Data)
				}
				return
			}
		case <-deadline:
			t.Fatal("超时未收到转发的日志事件")
		}
	}
}

func TestBindingFQNIsStable(t *testing.T) {
	// 前端用 Call.ByName("copywhere/internal/wailsui.Service.<Method>") 调用；
	// 包路径随模块名/目录变化会静默失效（表现为"点了没反应"），这里锁死它，
	// 同时确认关键方法仍被导出。
	ptr := reflect.TypeOf(&Service{})
	elem := ptr.Elem()
	fqn := elem.PkgPath() + "." + elem.Name()
	if fqn != "copywhere/internal/wailsui.Service" {
		t.Fatalf("绑定服务 FQN = %q，前端常量需要同步修改", fqn)
	}
	for _, m := range []string{"State", "Peers", "Records", "SendText", "SendFiles", "SaveSettings", "SetKVMNeighbor", "CopyText"} {
		if _, ok := ptr.MethodByName(m); !ok {
			t.Fatalf("绑定方法 %s 缺失", m)
		}
	}
}
