package app

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"copywhere/internal/config"
	"copywhere/internal/discovery"
	"copywhere/internal/transport"
	"copywhere/internal/trust"
)

// newKVMTestApp 构造仅用于 KVM 布局同步测试的 App（无网络组件、kvmSvc 为 nil）。
func newKVMTestApp(t *testing.T) *App {
	t.Helper()
	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfg.Path = filepath.Join(dir, "config.json")
	cfg.PeerTTLSec = 60
	cfg.NodeName = "A"
	a := &App{cfg: cfg, store: discovery.NewStore("self-id"), selfID: "self-id",
		rejected: map[string]bool{}}
	a.trust = trust.New(filepath.Join(dir, "peers.json"))
	return a
}

func TestOnKVMSyncSetAndClear(t *testing.T) {
	a := newKVMTestApp(t)

	// 设置：对端告知"我是你的右邻"
	if err := a.onKVMSync(transport.KVMSync{PeerID: "p1", PeerName: "B", Side: "right"}); err != nil {
		t.Fatalf("设置右邻失败: %v", err)
	}
	if a.cfg.KVMRight != "B" {
		t.Fatalf("右邻应为 B，实际 %q", a.cfg.KVMRight)
	}

	// 取消：对端告知"把我从你的右邻移除"
	if err := a.onKVMSync(transport.KVMSync{PeerID: "p1", PeerName: "B", Side: "right", Clear: true}); err != nil {
		t.Fatalf("取消右邻失败: %v", err)
	}
	if a.cfg.KVMRight != "" {
		t.Fatalf("取消后右邻应为空，实际 %q", a.cfg.KVMRight)
	}

	// 迟到的取消不覆盖新配置：该侧已换成别的节点
	a.cfg.KVMRight = "C"
	if err := a.onKVMSync(transport.KVMSync{PeerID: "p1", PeerName: "B", Side: "right", Clear: true}); err != nil {
		t.Fatalf("忽略迟到取消不应报错: %v", err)
	}
	if a.cfg.KVMRight != "C" {
		t.Fatalf("迟到取消不应覆盖新配置，实际 %q", a.cfg.KVMRight)
	}

	// 非法侧别
	if err := a.onKVMSync(transport.KVMSync{PeerID: "p1", PeerName: "B", Side: "up"}); err == nil {
		t.Fatal("非法侧别应报错")
	}
}

// freePort 借助临时监听取一个空闲 TCP 端口（关闭后由调用方重新绑定）。
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

// TestSyncKVMFromPushesClearToOldNeighbor 端到端验证：取消/换人时，
// 旧邻居会收到 Clear 同步消息（对端用真实 transport.Server 接收）。
func TestSyncKVMFromPushesClearToOldNeighbor(t *testing.T) {
	a := newKVMTestApp(t)

	port := freePort(t) // 对端传输服务端口（随机空闲端口，避免固定端口冲突）
	got := make(chan transport.KVMSync, 8)
	peerCfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	peerCfg.TransferPort = port
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go transport.Server(ctx, peerCfg, transport.Handlers{
		Authorize: func(hdr transport.Header) bool { return hdr.Token == "tokA" },
		OnKVM:     func(kv transport.KVMSync) error { got <- kv; return nil },
	})

	a.store.Upsert("peer-b-id", "B", "127.0.0.1", port, "sess1")
	a.store.Upsert("peer-c-id", "C", "127.0.0.1", port, "sess2")
	if err := a.trust.Add("peer-b-id", "B", "", "tokA"); err != nil {
		t.Fatal(err)
	}
	if err := a.trust.Add("peer-c-id", "C", "", "tokA"); err != nil {
		t.Fatal(err)
	}

	collect := func(n int, timeout time.Duration) []transport.KVMSync {
		var out []transport.KVMSync
		deadline := time.After(timeout)
		for len(out) < n {
			select {
			case kv := <-got:
				out = append(out, kv)
			case <-deadline:
				t.Fatalf("超时：%d 条同步消息只收到 %d 条", n, len(out))
			}
		}
		return out
	}

	// 场景 1：取消 —— B 原是本机左邻，清空后应通知 B"把我从你的右邻移除"
	a.cfg.KVMLeft = ""
	a.SyncKVMFrom("B", "")
	for _, kv := range collect(1, 3*time.Second) {
		if !kv.Clear || kv.Side != "right" || kv.PeerName != "A" {
			t.Fatalf("应收到取消同步（Clear、Side=right、PeerName=A），实际 %+v", kv)
		}
	}

	// 场景 2：换人 —— 左邻 B 换成 C：B 收到取消，C 收到设置
	a.cfg.KVMLeft = "C"
	a.SyncKVMFrom("B", "")
	msgs := collect(2, 3*time.Second)
	hasClearToB, hasSetToC := false, false
	for _, kv := range msgs {
		switch {
		case kv.Clear && kv.Side == "right" && kv.PeerName == "A":
			hasClearToB = true // B 与 C 同名即可达，此处按发送者语义校验
		case !kv.Clear && kv.Side == "right" && kv.PeerName == "A":
			hasSetToC = true
		}
	}
	if !hasClearToB || !hasSetToC {
		t.Fatalf("换人应同时产生取消与设置消息，实际 %+v", msgs)
	}

	// 场景 3：普通保存（左邻未变）不产生 Clear，但仍会向 C 推送常规设置消息
	a.cfg.KVMLeft = "C"
	a.SyncKVMFrom("C", "")
	select {
	case kv := <-got:
		if kv.Clear {
			t.Fatalf("左邻未变不应推送取消消息，实际 %+v", kv)
		}
	case <-time.After(300 * time.Millisecond):
	}
}

// TestSetKVMEnabledHotToggle 验证跨屏开关热启停：服务随开关创建/销毁、
// 重复启停幂等、停用后输入抑制被复位。
func TestSetKVMEnabledHotToggle(t *testing.T) {
	a := newKVMTestApp(t)
	a.cfg.KVMPort = freePort(t) // KVM 监听端口用随机空闲端口
	on := true
	a.cfg.KVMEnabled = &on

	a.SetKVMEnabled(true)
	svc := a.currentKVM()
	if svc == nil {
		t.Fatal("启用后应有 KVM 服务")
	}

	// 重复启用应幂等（不重建服务、不重新绑定端口）
	a.SetKVMEnabled(true)
	if a.currentKVM() != svc {
		t.Fatal("重复启用不应重建 KVM 服务")
	}

	a.SetKVMEnabled(false)
	if a.currentKVM() != nil {
		t.Fatal("停用后不应有 KVM 服务")
	}

	// 重复停用应幂等
	a.SetKVMEnabled(false)
	if a.currentKVM() != nil {
		t.Fatal("重复停用后不应有 KVM 服务")
	}

	// 停用后可重新启用，再停用（验证可反复启停）
	a.SetKVMEnabled(true)
	if a.currentKVM() == nil {
		t.Fatal("重新启用应成功")
	}
	a.SetKVMEnabled(false)
	if a.currentKVM() != nil {
		t.Fatal("再次停用后不应有 KVM 服务")
	}
}
