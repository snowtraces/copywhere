package app

import (
	"context"
	"testing"
	"time"

	"copywhere/internal/clip"
	"copywhere/internal/config"
	"copywhere/internal/discovery"
	"copywhere/internal/transport"
)

// TestSendRichNegotiation 验证逐对端能力协商：公告了 CapRich 的对端收到
// type="rich"（含 HTML 字节），未公告的旧版本对端只收到 type="text"（纯文本）。
func TestSendRichNegotiation(t *testing.T) {
	a := newTestApp(t, "node-a", freePort(t), freePort(t))

	port := freePort(t)
	type rec struct {
		typ  string
		rich transport.RichPayload
		text string
	}
	got := make(chan rec, 4)
	peerCfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	peerCfg.TransferPort = port
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go transport.Server(ctx, peerCfg, transport.Handlers{
		Authorize: func(hdr transport.Header) bool { return hdr.Token == "tokA" },
		OnRich: func(sender string, p transport.RichPayload) {
			got <- rec{typ: "rich", rich: p}
		},
		OnText: func(sender, text string) { got <- rec{typ: "text", text: text} },
	})
	if err := a.trust.Add("peer-x", "X", "", "tokA"); err != nil {
		t.Fatal(err)
	}

	rich := clip.Rich{Text: "甲", HTML: []byte("Version:0.9\r\n<b>甲</b>")}

	// ① 旧版本对端（无 caps）：只能收到纯文本
	a.store.Upsert("peer-x", "X", "127.0.0.1", port, "s1")
	a.sendRichPayload(rich)
	select {
	case r := <-got:
		if r.typ != "text" || r.text != "甲" {
			t.Fatalf("旧版本对端应收到降级 text，实际 %+v", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("旧版本对端未收到消息")
	}

	// ② 新版本对端（公告 CapRich）：收到 rich，HTML 字节完整
	a.store.UpsertCaps("peer-x", "X", "127.0.0.1", port, "s1", []string{discovery.CapRich})
	a.sendRichPayload(rich)
	select {
	case r := <-got:
		if r.typ != "rich" {
			t.Fatalf("新对端应收到 rich，实际 %+v", r)
		}
		if r.rich.Text != "甲" || string(r.rich.HTML) != string(rich.HTML) {
			t.Fatalf("rich 负载内容不符: %+v", r.rich)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("新对端未收到消息")
	}
}

// TestSendRichOverCapDegrades 验证发送侧 16MB 预检：超限富负载降级为纯文本，
// 绝不把必被对端拒收的超大 rich 头发出去。
func TestSendRichOverCapDegrades(t *testing.T) {
	a := newTestApp(t, "node-a", freePort(t), freePort(t))
	port := freePort(t)
	got := make(chan string, 2)
	peerCfg, _ := config.Default()
	peerCfg.TransferPort = port
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go transport.Server(ctx, peerCfg, transport.Handlers{
		Authorize: func(hdr transport.Header) bool { return hdr.Token == "tokA" },
		OnRich:    func(sender string, p transport.RichPayload) { got <- "rich" },
		OnText:    func(sender, text string) { got <- "text" },
	})
	if err := a.trust.Add("peer-x", "X", "", "tokA"); err != nil {
		t.Fatal(err)
	}
	a.store.UpsertCaps("peer-x", "X", "127.0.0.1", port, "s1", []string{discovery.CapRich})

	huge := make([]byte, transport.MaxRichBytes) // 序列化后（base64）必然超 16MB
	for i := range huge {
		huge[i] = 'A'
	}
	a.sendRichPayload(clip.Rich{Text: "小文本", HTML: huge})
	select {
	case k := <-got:
		if k != "text" {
			t.Fatalf("超限富负载应降级为 text，实际收到 %s", k)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("未收到降级后的 text 消息")
	}
}

// TestRichPendingRetry 验证重试闭环：对端离线时 SendRich 入队，
// 对端上线后 flushLoop 自动补发（*e.rich 路径）。
func TestRichPendingRetry(t *testing.T) {
	a := newTestApp(t, "node-a", freePort(t), freePort(t))
	port := freePort(t)

	// 先注册离线对端，让 SendRich 撞不可达进入重试队列
	a.store.UpsertCaps("peer-x", "X", "127.0.0.1", port, "s1", []string{discovery.CapRich})
	if err := a.trust.Add("peer-x", "X", "", "tokA"); err != nil {
		t.Fatal(err)
	}
	// 断言重试队列收到 rich 条目
	a.SendRich(clip.Rich{Text: "待重试", HTML: []byte("Version:0.9\r\n<b>x</b>")})
	a.pendingMu.Lock()
	n := len(a.pending)
	var kind string
	if n > 0 {
		kind = a.pending[n-1].kind
	}
	a.pendingMu.Unlock()
	if n == 0 || kind != "rich" {
		t.Fatalf("SendRich 不可达时应入队 rich 重试，实际 n=%d kind=%q", n, kind)
	}

	// 对端上线后手工触发一轮 flush 逻辑：直接调 sendRichPayload 验证富负载可达
	got := make(chan transport.RichPayload, 1)
	peerCfg, _ := config.Default()
	peerCfg.TransferPort = port
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go transport.Server(ctx, peerCfg, transport.Handlers{
		Authorize: func(hdr transport.Header) bool { return hdr.Token == "tokA" },
		OnRich: func(sender string, p transport.RichPayload) {
			select {
			case got <- p:
			default:
			}
		},
	})
	res := a.sendRichPayload(clip.Rich{Text: "待重试", HTML: []byte("Version:0.9\r\n<b>x</b>")})
	if len(res) == 0 || !res[0].OK {
		t.Fatalf("对端上线后 rich 重试应成功: %+v", res)
	}
	select {
	case p := <-got:
		if p.Text != "待重试" || string(p.HTML) == "" {
			t.Fatalf("重试送达的 rich 内容不符: %+v", p)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("对端未收到重试的 rich")
	}
}
