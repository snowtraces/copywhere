package app

import (
	"path/filepath"
	"testing"
	"time"
)

// waitBatch 等待一次合并回调（超时即失败）。
func waitBatch(t *testing.T, ch <-chan []string) []string {
	t.Helper()
	select {
	case paths := <-ch:
		return paths
	case <-time.After(2 * time.Second):
		t.Fatal("合并窗口到期后应有回调")
		return nil
	}
}

// TestPathBatcherMergesWindow 覆盖右键菜单多选的落地形状：资源管理器按"每项一次"
// 分发，很短时间内的多次投递必须合并成一次发送，且同批内去重。
func TestPathBatcherMergesWindow(t *testing.T) {
	ch := make(chan []string, 4)
	b := newPathBatcher(30*time.Millisecond, time.Second, func(paths []string) { ch <- paths })

	b.Add([]string{`C:\a\1.txt`})
	b.Add([]string{`C:\a\2.txt`})
	b.Add([]string{`C:\a\2.txt`}) // 同一路径重复投递
	b.Add(nil)                    // 空投递不改变状态

	paths := waitBatch(t, ch)
	if len(paths) != 2 || paths[0] != `C:\a\1.txt` || paths[1] != `C:\a\2.txt` {
		t.Fatalf("应合并为去重后的 2 个路径，实际 %v", paths)
	}
	select {
	case extra := <-ch:
		t.Fatalf("同一窗口不应触发第二次回调: %v", extra)
	case <-time.After(120 * time.Millisecond):
	}
}

// TestPathBatcherSeparateWindows 验证静默窗口：前一批发出后，
// 新的投递会重新开启一个窗口，不会互相吞掉。
func TestPathBatcherSeparateWindows(t *testing.T) {
	ch := make(chan []string, 4)
	b := newPathBatcher(20*time.Millisecond, time.Second, func(paths []string) { ch <- paths })

	b.Add([]string{"a"})
	if got := waitBatch(t, ch); len(got) != 1 || got[0] != "a" {
		t.Fatalf("首批应为 [a]，实际 %v", got)
	}
	b.Add([]string{"b"})
	if got := waitBatch(t, ch); len(got) != 1 || got[0] != "b" {
		t.Fatalf("第二批应为 [b]，实际 %v", got)
	}
}

// TestPathBatcherMaxWait 验证硬上界：持续有新投递时，最迟在首批之后 maxWait
// 就必须发出（多选很多、进程唤起很慢时也不会无限攒）。
func TestPathBatcherMaxWait(t *testing.T) {
	ch := make(chan []string, 4)
	b := newPathBatcher(150*time.Millisecond, 100*time.Millisecond, func(paths []string) { ch <- paths })

	b.Add([]string{"1"})
	// 持续投递：每 30ms 来一个，若没有上界就会被无限后延
	stop := make(chan struct{})
	go func() {
		for i := 2; i <= 6; i++ {
			select {
			case <-stop:
				return
			case <-time.After(30 * time.Millisecond):
			}
			b.Add([]string{string(rune('0' + i))})
		}
	}()
	defer close(stop)

	start := time.Now()
	paths := waitBatch(t, ch)
	elapsed := time.Since(start)
	if len(paths) < 2 {
		t.Fatalf("上界触发前应已并入多个路径，实际 %v", paths)
	}
	if elapsed > 250*time.Millisecond {
		t.Fatalf("应在 maxWait 附近发出（实际 %s）", elapsed)
	}
}

// TestShellPathsPreview 覆盖日志里的一行摘要（最多 3 个名字 + 剩余计数）。
func TestShellPathsPreview(t *testing.T) {
	paths := []string{
		filepath.Join("x", "a.txt"),
		filepath.Join("x", "b.txt"),
		filepath.Join("x", "c.txt"),
		filepath.Join("x", "d.txt"),
	}
	if got, want := shellPathsPreview(paths), "a.txt、b.txt、c.txt、+1"; got != want {
		t.Fatalf("摘要应为 %q，实际 %q", want, got)
	}
	if got := shellPathsPreview(nil); got != "" {
		t.Fatalf("空列表摘要应为空，实际 %q", got)
	}
}
