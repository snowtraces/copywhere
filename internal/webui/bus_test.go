package webui

import (
	"testing"
	"time"

	"copywhere/internal/app"
	"copywhere/internal/ui"
)

func TestBusLogBuffering(t *testing.T) {
	b := NewBus()
	for i := 0; i < maxLogLines+50; i++ {
		b.Log("line")
	}
	if got := len(b.Logs()); got != maxLogLines {
		t.Fatalf("日志缓存应裁剪到 %d 行，实际 %d", maxLogLines, got)
	}
}

func TestBusFileBuffering(t *testing.T) {
	b := NewBus()
	for i := 0; i < maxFileRecs+10; i++ {
		b.AddFile(ui.FileRecord{Time: time.Now(), Name: "f", Size: 1})
	}
	if got := len(b.Files()); got != maxFileRecs {
		t.Fatalf("记录缓存应裁剪到 %d 条，实际 %d", maxFileRecs, got)
	}
}

func TestBusSubscribeReceivesLiveEvents(t *testing.T) {
	b := NewBus()
	ch, cancel := b.Subscribe()
	defer cancel()

	b.Log("hello")
	b.AddFile(ui.FileRecord{Time: time.Now(), Name: "a.txt", Size: 3, In: true})
	b.Progress(app.Progress{Peer: "p", Name: "a.txt", Sent: 1, Total: 3})

	want := []string{EvLog, EvFile, EvProgress}
	for _, wt := range want {
		select {
		case ev := <-ch:
			if ev.Type != wt {
				t.Fatalf("期望事件 %s，收到 %s", wt, ev.Type)
			}
		case <-time.After(time.Second):
			t.Fatalf("等待事件 %s 超时", wt)
		}
	}
}

func TestBusPublishDropsWhenSubscriberSlow(t *testing.T) {
	b := NewBus()
	_, cancel := b.Subscribe()
	defer cancel()
	for i := 0; i < chBuffer*2; i++ {
		b.Log("flood") // 消费者不读，publish 必须非阻塞
	}
	if b.Subscribers() != 1 {
		t.Fatalf("订阅者数应为 1，实际 %d", b.Subscribers())
	}
}

func TestBusCancel(t *testing.T) {
	b := NewBus()
	_, cancel := b.Subscribe()
	cancel()
	if got := b.Subscribers(); got != 0 {
		t.Fatalf("取消后订阅者数应为 0，实际 %d", got)
	}
}
