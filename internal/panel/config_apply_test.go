package panel

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"copywhere/internal/config"
	"copywhere/internal/discovery"
	"copywhere/internal/trust"
)

// fakeCore 是最小 Core 实现，用于验证 ApplyConfig 的副作用。
type fakeCore struct {
	mu sync.Mutex

	kvmEnabledCalls []bool
	touchOnCalls    []bool
	touchSpeedCalls []int

	kvmSyncOldLeft  string
	kvmSyncOldRight string
	kvmSyncCalls    int
}

func (f *fakeCore) Peers() []discovery.Peer    { return nil }
func (f *fakeCore) SelfName() string           { return "test" }
func (f *fakeCore) SelfIP() string             { return "127.0.0.1" }
func (f *fakeCore) SetPaused(bool)             {}
func (f *fakeCore) IsPaused() bool             { return false }
func (f *fakeCore) SendFiles([]string, int64)  {}
func (f *fakeCore) SendText(string)            {}
func (f *fakeCore) PairedWith(string) bool     { return false }
func (f *fakeCore) RejectedWith(string) bool   { return false }
func (f *fakeCore) PairedList() []trust.Paired { return nil }
func (f *fakeCore) PairWith(string) error      { return nil }
func (f *fakeCore) RespondPair(bool) error     { return nil }
func (f *fakeCore) PendingPair() (string, string, bool) {
	return "", "", false
}
func (f *fakeCore) Unpair(string) error { return nil }
func (f *fakeCore) SyncKVMFrom(oldLeft, oldRight string) {
	f.mu.Lock()
	f.kvmSyncOldLeft, f.kvmSyncOldRight = oldLeft, oldRight
	f.kvmSyncCalls++
	f.mu.Unlock()
}
func (f *fakeCore) SetKVMEnabled(on bool) {
	f.mu.Lock()
	f.kvmEnabledCalls = append(f.kvmEnabledCalls, on)
	f.mu.Unlock()
}
func (f *fakeCore) SetTouchpadGestures(on bool) {
	f.mu.Lock()
	f.touchOnCalls = append(f.touchOnCalls, on)
	f.mu.Unlock()
}
func (f *fakeCore) SetTouchpadSpeed(pct int) {
	f.mu.Lock()
	f.touchSpeedCalls = append(f.touchSpeedCalls, pct)
	f.mu.Unlock()
}
func (f *fakeCore) WatchMode() string { return "listener" }

func (f *fakeCore) syncSnapshot() (string, string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.kvmSyncOldLeft, f.kvmSyncOldRight, f.kvmSyncCalls
}

func newApplyFixture(t *testing.T) (*config.Config, string, *fakeCore) {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.json")
	cfg.Path = cfgPath
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	return cfg, cfgPath, &fakeCore{}
}

func TestApplyConfigSavesAndHotApplies(t *testing.T) {
	cfg, cfgPath, core := newApplyFixture(t)

	err := ApplyConfig(core, cfg, cfgPath, map[string]any{
		"node_name":             "NEW-NODE",
		"max_auto_copy_mb":      float64(5),
		"auto_paste":            false,
		"text_sync":             false,
		"kvm_enabled":           false,
		"kvm_speed_percent":     float64(120),
		"kvm_touchpad_gestures": true,
		"kvm_touchpad_speed":    float64(150),
		"kvm_move_interval_ms":  float64(10),
	})
	if err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}

	if cfg.NodeName != "NEW-NODE" || cfg.MaxAutoCopyMB != 5 || cfg.AutoPaste || cfg.TextSync {
		t.Fatalf("内存配置未更新: %+v", cfg)
	}
	if cfg.KVMSpeedPct != 120 || !cfg.KVMTouchpadOn() || cfg.KVMTouchpadSpeedPct() != 150 {
		t.Fatalf("KVM 配置未更新: %+v", cfg)
	}
	if len(core.kvmEnabledCalls) != 1 || core.kvmEnabledCalls[0] {
		t.Fatalf("kvm_enabled 应触发热停用，实际 %v", core.kvmEnabledCalls)
	}
	if len(core.touchOnCalls) != 1 || !core.touchOnCalls[0] {
		t.Fatalf("触控板开关应触发热更新，实际 %v", core.touchOnCalls)
	}
	if len(core.touchSpeedCalls) != 1 || core.touchSpeedCalls[0] != 150 {
		t.Fatalf("触控板速度应触发热更新，实际 %v", core.touchSpeedCalls)
	}

	// 邻居改动走异步 SyncKVMFrom（携带旧值）
	deadline := time.Now().Add(time.Second)
	for {
		_, _, n := core.syncSnapshot()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("KVM 邻居变更未触发 SyncKVMFrom")
		}
		time.Sleep(10 * time.Millisecond)
	}

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk config.Config
	if err := json.Unmarshal(data, &onDisk); err != nil {
		t.Fatal(err)
	}
	if onDisk.NodeName != "NEW-NODE" || onDisk.MaxAutoCopyMB != 5 || onDisk.KVMSpeedPct != 120 {
		t.Fatalf("落盘配置不符: %+v", onDisk)
	}
}

func TestApplyConfigValidation(t *testing.T) {
	cfg, cfgPath, core := newApplyFixture(t)
	before := cfg.NodeName

	err := ApplyConfig(core, cfg, cfgPath, map[string]any{
		"max_auto_copy_mb":   float64(-1),
		"kvm_speed_percent":  float64(9999),
		"kvm_touchpad_speed": float64(1),
	})
	if err == nil {
		t.Fatal("非法配置应报错")
	}
	for _, want := range []string{"max_auto_copy_mb", "0~500", "10~1000"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误信息缺少 %q: %v", want, err)
		}
	}
	if cfg.NodeName != before {
		t.Fatal("校验失败不应改动其它字段")
	}
	if len(core.kvmEnabledCalls) != 0 {
		t.Fatal("校验失败不应触发副作用")
	}
}

func TestApplyConfigKVMNeighborsCarryOldValues(t *testing.T) {
	cfg, cfgPath, core := newApplyFixture(t)
	cfg.KVMLeft, cfg.KVMRight = "OLD-L", "OLD-R"

	err := ApplyConfig(core, cfg, cfgPath, map[string]any{"kvm_left": "NEW-L", "kvm_right": ""})
	if err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	if cfg.KVMLeft != "NEW-L" || cfg.KVMRight != "" {
		t.Fatalf("邻居未更新: %+v", cfg)
	}
	deadline := time.Now().Add(time.Second)
	for {
		oldL, oldR, n := core.syncSnapshot()
		if n > 0 {
			if oldL != "OLD-L" || oldR != "OLD-R" {
				t.Fatalf("旧邻居未传对: %q/%q", oldL, oldR)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("未触发 SyncKVMFrom")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestApplyConfigEmptyMapPersists(t *testing.T) {
	cfg, cfgPath, core := newApplyFixture(t)
	if err := ApplyConfig(core, cfg, cfgPath, map[string]any{}); err != nil {
		t.Fatalf("空修改也应成功落盘: %v", err)
	}
	if _, err := os.Stat(cfgPath); err != nil {
		t.Fatalf("配置文件不存在: %v", err)
	}
}
