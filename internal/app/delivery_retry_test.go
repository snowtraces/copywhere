package app

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"copywhere/internal/config"
	"copywhere/internal/transport"
)

// TestUnpairedThirdNodeNotDelivered 回归：在线但未配对的第三个节点不参与投递。
//
// 曾经的缺陷：未配对节点被算进"可投递节点"，每次发送都因它失败而被判为可重试，
// 于是整包内容在 60s 内每 3s 重发一遍——一次复制，已配对的对端收到十几份。
func TestUnpairedThirdNodeNotDelivered(t *testing.T) {
	a := newTestApp(t, "node-a", freePort(t), freePort(t))
	a.cfg.PeerTTLSec = 120

	// 已配对的好节点：真实 transport 服务，记录每次投递
	port := freePort(t)
	got := make(chan string, 16)
	peerCfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	peerCfg.TransferPort = port
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go transport.Server(ctx, peerCfg, transport.Handlers{
		Authorize: func(hdr transport.Header) bool { return hdr.Token == "tokA" },
		OnText:    func(sender, text string) { got <- text },
	})
	if err := a.trust.Add("peer-good", "GOOD", "", "tokA"); err != nil {
		t.Fatal(err)
	}
	a.store.Upsert("peer-good", "GOOD", "127.0.0.1", port, "s1")

	// 第三个节点：在线、未配对。用只计数不说话的监听器代表它：
	// 若它仍参与投递，这里会收到连接。
	var thirdAccepts int32
	thirdLn, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer thirdLn.Close()
	go func() {
		for {
			c, aerr := thirdLn.Accept()
			if aerr != nil {
				return
			}
			atomic.AddInt32(&thirdAccepts, 1)
			c.Close()
		}
	}()
	a.store.Upsert("peer-third", "THIRD", "127.0.0.1", thirdLn.Addr().(*net.TCPAddr).Port, "s2")

	runCtx, stop := context.WithCancel(context.Background())
	defer stop()
	go a.flushLoop(runCtx)

	a.SendText("hello-once")

	// 给 flushLoop 至少两个节拍：修好后不应有任何补发
	deadline := time.After(8 * time.Second)
	delivered := 0
loop:
	for {
		select {
		case <-got:
			delivered++
		case <-deadline:
			break loop
		}
	}

	a.pendingMu.Lock()
	pend := len(a.pending)
	a.pendingMu.Unlock()

	if delivered != 1 {
		t.Fatalf("已配对节点应只收到 1 份，实际 %d 份（未配对节点触发了重复投递）", delivered)
	}
	if n := atomic.LoadInt32(&thirdAccepts); n != 0 {
		t.Fatalf("未配对节点不应参与投递，实际收到 %d 次连接", n)
	}
	if pend != 0 {
		t.Fatalf("全部成功时不应有补发队列，实际 %d 条", pend)
	}
}

// TestDeliveryFailureRetriesOnce 回归：投递失败只补发一次（总尝试次数 = 2），
// 之后不再重试——避免失败对端每 3s 被重发、界面上刷出十几条同样的失败记录。
func TestDeliveryFailureRetriesOnce(t *testing.T) {
	a := newTestApp(t, "node-a", freePort(t), freePort(t))
	a.cfg.PeerTTLSec = 120

	// 一个"接受即关闭"的对端：每次尝试都会留下一次 accept
	var accepts int32
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			atomic.AddInt32(&accepts, 1)
			c.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	if err := a.trust.Add("peer-bad", "BAD", "", "tokB"); err != nil {
		t.Fatal(err)
	}
	a.store.Upsert("peer-bad", "BAD", "127.0.0.1", port, "s1")

	runCtx, stop := context.WithCancel(context.Background())
	defer stop()
	go a.flushLoop(runCtx)

	a.SendText("will-fail")

	// 首次尝试 1 次 + 补发 1 次 = 2 次；3s 一个节拍，留足 8s 观察不再重试
	time.Sleep(8 * time.Second)

	gotN := atomic.LoadInt32(&accepts)
	a.pendingMu.Lock()
	pend := len(a.pending)
	a.pendingMu.Unlock()

	if gotN != 2 {
		t.Fatalf("失败投递应只重试一次（共 2 次尝试），实际 %d 次", gotN)
	}
	if pend != 0 {
		t.Fatalf("补发一次后应清空队列，实际仍有 %d 条", pend)
	}
}
