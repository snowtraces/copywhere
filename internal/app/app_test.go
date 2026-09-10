package app

import (
	"bytes"
	"context"
	"net"
	"runtime"
	"testing"
	"time"

	"copywhere/internal/config"
	"copywhere/internal/ui"
)

type fakeSink struct {
	logs      []string
	lastLog   string
	lastProg  Progress
	progCount int
}

func (f *fakeSink) Log(line string)             { f.lastLog = line; f.logs = append(f.logs, line) }
func (f *fakeSink) AddFile(r ui.FileRecord)     {}
func (f *fakeSink) Progress(p Progress)         { f.lastProg = p; f.progCount++ }
func (f *fakeSink) PairRequest(id, name string) {}

func TestSinkWriterLineSplitting(t *testing.T) {
	s := &fakeSink{}
	w := &sinkWriter{sink: s}
	w.Write([]byte("first line\nsecond line\npar"))
	w.Write([]byte("tial line\n"))
	want := []string{"first line", "second line", "partial line"}
	if len(s.logs) != len(want) {
		t.Fatalf("应产生 %d 条日志，实际 %d: %v", len(want), len(s.logs), s.logs)
	}
	for i, l := range want {
		if s.logs[i] != l {
			t.Errorf("第 %d 行应为 %q，实际 %q", i, l, s.logs[i])
		}
	}
}

func TestSinkWriterTrimsCR(t *testing.T) {
	s := &fakeSink{}
	w := &sinkWriter{sink: s}
	w.Write([]byte("with cr\r\n"))
	if s.lastLog != "with cr" {
		t.Fatalf("应去除行尾 \\r，实际 %q", s.lastLog)
	}
}

func TestProgressWriterThrottle(t *testing.T) {
	s := &fakeSink{}
	total := int64(1024 * 1024) // 1MB
	pw := &progressWriter{w: &bytes.Buffer{}, total: total, peer: "p", name: "f.bin", sink: s}

	// 写入 300KB → 触发 1 次上报（越过第一个 256KB 阈值）
	chunk := make([]byte, 300*1024)
	pw.Write(chunk)
	if s.progCount != 1 {
		t.Fatalf("300KB 后应上报 1 次，实际 %d", s.progCount)
	}
	if s.lastProg.Sent != int64(300*1024) || s.lastProg.Total != total {
		t.Fatalf("进度数值不符: %+v", s.lastProg)
	}

	// 再写 100KB（累计 400KB，未到 512KB 阈值）→ 不上报
	pw.Write(make([]byte, 100*1024))
	if s.progCount != 1 {
		t.Fatalf("未越阈值不应上报，实际 %d 次", s.progCount)
	}

	// 写满剩余字节 → 在最后阈值处再上报（writer 只做节流，
	// 终态事件由 sendFilePayload 在发送结束时显式补发）
	for pw.sent < total {
		n := int64(256 * 1024)
		if total-pw.sent < n {
			n = total - pw.sent
		}
		pw.Write(make([]byte, n))
	}
	// 400KB 起步，之后每次越过一个 256KB 阈值上报一次：
	// 671744 → 933888，最后 114688 字节未达阈值不再上报
	if s.progCount != 3 {
		t.Fatalf("应上报 3 次，实际 %d", s.progCount)
	}
	if s.lastProg.Sent != 933888 {
		t.Fatalf("最后一次上报应在 933888 字节处，实际 %d", s.lastProg.Sent)
	}
}

// TestStartKeepsContextAlive 是回归测试：Start 立即返回后，
// 子 context 必须保持存活、传输端口保持监听（否则表现为 TUI 秒退、对端无法握手）。
func TestStartKeepsContextAlive(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("仅 Windows（剪贴板/注册表依赖）")
	}
	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	cfg.DiscoveryPort = 48841
	cfg.TransferPort = 48842
	cfg.KVMPort = 48843
	kvmOff := false
	cfg.KVMEnabled = &kvmOff

	a, err := Start(context.Background(), cfg, Options{})
	if err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	defer a.cancel() // 不调用 Wait（会阻塞），直接释放子 context

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if a.ctx.Err() != nil {
			t.Fatal("Start 返回后子 context 被提前取消（传输/发现/TUI 会全部秒退）")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if a.ctx.Err() != nil {
		t.Fatal("Start 返回后子 context 被提前取消")
	}
	// 传输端口仍在监听：能建立 TCP 连接即证明服务存活
	conn, err := net.DialTimeout("tcp", "127.0.0.1:48842", time.Second)
	if err != nil {
		t.Fatalf("传输端口未监听（服务已死）: %v", err)
	}
	conn.Close()
}
