package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"copywhere/internal/config"
	"copywhere/internal/transport"
)

// newTestApp 构造一个使用独立临时目录（配置+信任库）的 App。
// 同进程内 nodeID() 只算一次（MachineGuid），测试用 stubID 注入不同指纹。
func stubID(t *testing.T, id string) {
	t.Helper()
	old := nodeID
	nodeID = func() string { return id }
	t.Cleanup(func() { nodeID = old })
}

func newTestApp(t *testing.T, selfID string, discPort, xferPort int) *App {
	t.Helper()
	stubID(t, selfID)
	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	cfg.DiscoveryPort = discPort
	cfg.TransferPort = xferPort
	cfg.KVMPort = 0
	kvmOff := false
	cfg.KVMEnabled = &kvmOff
	cfg.Path = filepath.Join(t.TempDir(), "config.json")
	a, err := newApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// TestPairingFlow 走完整配对流程（同机回环）：A 发起 → B 接受 → 双方入库 →
// 以配对令牌完成一次文本传输；并验证未配对/错误令牌被拒。
func TestPairingFlow(t *testing.T) {
	// B：被请求方，端口 488xx 段
	b := newTestApp(t, "node-b", 48861, 48862)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gotText := make(chan string, 1)
	go transport.Server(ctx, b.cfg, transport.Handlers{
		OnText:    func(sender, text string) { gotText <- text },
		Authorize: b.authorize,
		OnPair:    b.onPairRequest,
	})

	// B 端自动接受：轮询到待裁决请求即同意
	go func() {
		for i := 0; i < 100; i++ {
			if _, _, ok := b.PendingPair(); ok {
				b.RespondPair(true)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	// A：发起方，把 B 注册进节点表后发起配对
	a := newTestApp(t, "node-a", 48863, 48864)
	a.store.Upsert(b.selfID, "节点B", "127.0.0.1", b.cfg.TransferPort, "sess")
	if err := a.PairWith(b.selfID); err != nil {
		t.Fatalf("配对失败: %v", err)
	}

	// 双方都应有配对记录
	if !a.PairedWith(b.selfID) || !b.PairedWith(a.selfID) {
		t.Fatal("配对后双方应各有一条记录")
	}
	ea, _ := a.trust.Get(b.selfID)
	eb, _ := b.trust.Get(a.selfID)
	if ea.PeerToken == "" || eb.PairToken == "" {
		t.Fatalf("令牌缺失: A/B = %+v / %+v", ea, eb)
	}
	if len(a.PairedList()) != 1 || len(b.PairedList()) != 1 {
		t.Fatal("PairedList 应各有一条")
	}

	// A 以配对令牌发送文本，B 应能收到
	a.sendTextPayload("hello-pair")
	select {
	case got := <-gotText:
		if got != "hello-pair" {
			t.Fatalf("文本内容不符: %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("配对后发送文本超时未到达")
	}

	// B 解除配对后，A 用旧令牌应被拒
	if err := b.Unpair(a.selfID); err != nil {
		t.Fatal(err)
	}
	a.sendTextPayload("should-fail")
	select {
	case got := <-gotText:
		t.Fatalf("解除配对后不应收到: %q", got)
	case <-time.After(1 * time.Second):
	}
	if !a.RejectedWith(b.selfID) {
		t.Fatal("被拒后 A 应标记该节点暂停发送")
	}
}

// TestUnpairedRejected 废弃共享 token 后：未配对节点即使曾配置相同 token 也必须被拒。
func TestUnpairedRejected(t *testing.T) {
	b := newTestApp(t, "node-b2", 48865, 48866)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gotText := make(chan string, 1)
	go transport.Server(ctx, b.cfg, transport.Handlers{
		OnText:    func(sender, text string) { gotText <- text },
		Authorize: b.authorize,
		OnPair:    b.onPairRequest,
	})

	a := newTestApp(t, "node-a2", 48867, 48868)
	a.store.Upsert(b.selfID, "节点B", "127.0.0.1", b.cfg.TransferPort, "sess")
	// 未配对：sendToPeer 应直接拒绝
	peer := a.store.Alive(a.ttl())[0]
	data := []byte("nope")
	sum := sha256.Sum256(data)
	hdr := transport.Header{
		V: 1, Type: "text", Name: "text", Size: int64(len(data)),
		SHA256: hex.EncodeToString(sum[:]), Sender: a.cfg.NodeName,
	}
	_, err := a.sendToPeer(peer, hdr, func() (func(io.Writer) error, error) {
		return func(w io.Writer) error { _, err := w.Write(data); return err }, nil
	}, 5*time.Second)
	if err == nil {
		t.Fatal("未配对发送应被拒绝")
	}
	if !strings.Contains(err.Error(), "未配对") {
		t.Fatalf("错误信息应提示未配对: %v", err)
	}
	select {
	case got := <-gotText:
		t.Fatalf("未配对不应收到内容: %q", got)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestRespondPairNoPending 空裁决应报错。
func TestRespondPairNoPending(t *testing.T) {
	a := newTestApp(t, "node-x", 48869, 48870)
	if err := a.RespondPair(true); err == nil {
		t.Fatal("无待裁决请求时应报错")
	}
}
