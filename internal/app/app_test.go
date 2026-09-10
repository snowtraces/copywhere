package app

import (
	"archive/zip"
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
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

// buildTestZip 生成测试用 zip：一个含子目录的文件夹 + 一个顶层文件。
func buildTestZip(t *testing.T, path string, entries map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestUnpackZip 验证合成 zip 的自动解包：解到所在目录、zip 本体删除、
// 返回顶层条目，且目录/多文件两类 bundle 都能正确还原。
func TestUnpackZip(t *testing.T) {
	t.Run("文件夹包", func(t *testing.T) {
		dir := t.TempDir()
		zipPath := filepath.Join(dir, "copywhere-bundle-0102-150405.zip")
		buildTestZip(t, zipPath, map[string]string{
			"MyProject/src/main.go":  "package main",
			"MyProject/README.md":    "hello",
			"MyProject/docs/api.txt": "api",
		})
		tops, err := unpackZip(zipPath)
		if err != nil {
			t.Fatalf("解包失败: %v", err)
		}
		if _, err := os.Stat(zipPath); !os.IsNotExist(err) {
			t.Fatal("解包成功后 zip 本体应被删除")
		}
		if len(tops) != 1 || filepath.Base(tops[0]) != "MyProject" {
			t.Fatalf("文件夹包应解出单个顶层目录: %v", tops)
		}
		got, err := os.ReadFile(filepath.Join(tops[0], "src", "main.go"))
		if err != nil || string(got) != "package main" {
			t.Fatalf("解包内容不一致: %v", err)
		}
	})

	t.Run("多文件包", func(t *testing.T) {
		dir := t.TempDir()
		zipPath := filepath.Join(dir, "copywhere-bundle-0102-150406.zip")
		buildTestZip(t, zipPath, map[string]string{
			"b.txt":      "B",
			"sub/c.txt":  "C",
			"notes .txt": "N",
		})
		tops, err := unpackZip(zipPath)
		if err != nil {
			t.Fatalf("解包失败: %v", err)
		}
		if len(tops) != 3 {
			t.Fatalf("多文件包应解出 3 个顶层条目（含保留的子目录）: %v", tops)
		}
		if _, err := os.ReadFile(filepath.Join(dir, "b.txt")); err != nil {
			t.Fatalf("顶层文件应解到接收子目录根: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "sub", "c.txt")); err != nil {
			t.Fatalf("子目录结构应保留: %v", err)
		}
	})

	t.Run("拒绝伪装路径穿越", func(t *testing.T) {
		dir := t.TempDir()
		zipPath := filepath.Join(dir, "evil2.zip")
		buildTestZip(t, zipPath, map[string]string{
			"../escape.txt": "bad",
			".. /evil.txt":  "bad", // 尾部空格伪装
			".../evil2.txt": "bad", // 多点伪装
			" . /evil3.txt": "bad", // 点空格混合伪装
		})
		if _, err := unpackZip(zipPath); err == nil {
			t.Fatal("含伪装 .. 的 zip 条目应被拒绝")
		}
		parent := filepath.Dir(dir)
		for _, leak := range []string{"escape.txt", "evil.txt", "evil2.txt", "evil3.txt"} {
			if _, err := os.Stat(filepath.Join(parent, leak)); err == nil {
				t.Fatalf("不应解包到目录之外: %s", leak)
			}
		}
	})

	t.Run("保留前导点文件名", func(t *testing.T) {
		dir := t.TempDir()
		zipPath := filepath.Join(dir, "dot.zip")
		buildTestZip(t, zipPath, map[string]string{
			"MyProject/.gitignore": "build/",
			"MyProject/pkg.a.":     "trailing dot",
		})
		tops, err := unpackZip(zipPath)
		if err != nil {
			t.Fatalf("解包失败: %v", err)
		}
		if _, err := os.ReadFile(filepath.Join(tops[0], ".gitignore")); err != nil {
			t.Fatalf("前导点文件名应保留: %v", err)
		}
		// Windows 非法的尾部点被剥掉，文件仍解出且不会改名成 (n)
		if _, err := os.Stat(filepath.Join(tops[0], "pkg.a")); err != nil {
			t.Fatalf("尾部点应被剥掉后正常解出: %v", err)
		}
	})
}

// TestBundleRecordPath 验证解包后记录映射：文件夹包指向解出的目录本身，
// 多文件包指向接收子目录——都不应指向已被删除的 zip。
func TestBundleRecordPath(t *testing.T) {
	if got := bundleRecordPath([]string{`D:\recv\20260910-120000-A\MyProject`}); filepath.Base(got) != "MyProject" {
		t.Fatalf("单目标应指向目标本身: %q", got)
	}
	dir := `D:\recv\20260910-120001-A`
	got := bundleRecordPath([]string{filepath.Join(dir, "a.txt"), filepath.Join(dir, "b.txt")})
	if got != dir {
		t.Fatalf("多目标应指向接收子目录: %q", got)
	}
	if got := bundleRecordPath(nil); got != "" {
		t.Fatalf("空目标应返回空串: %q", got)
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
