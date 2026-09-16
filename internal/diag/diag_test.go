package diag

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestRingWrapsAndOrders(t *testing.T) {
	var r ring
	for i := 0; i < ringCap+10; i++ {
		r.add("t", "m")
	}
	all := r.all()
	if len(all) != ringCap {
		t.Fatalf("环形日志应封顶 %d 条，实际 %d", ringCap, len(all))
	}
	// 旧在前：时间戳单调不减
	for i := 1; i < len(all); i++ {
		if all[i].At.Before(all[i-1].At) {
			t.Fatal("诊断日志顺序应为旧在前")
		}
	}
}

func TestCounters(t *testing.T) {
	Add("test.x", 5)
	Add("test.x", -2)
	if got := Counter("test.x"); got != 3 {
		t.Fatalf("计数器应为 3，实际 %d", got)
	}
	if got := Counter("test.not-exist"); got != 0 {
		t.Fatalf("不存在的计数器应为 0，实际 %d", got)
	}
	snap := Snap()
	if snap.Counters["test.x"] != 3 {
		t.Fatalf("快照应包含计数器: %+v", snap.Counters)
	}
}

func TestSnapshotJSON(t *testing.T) {
	Logf("unit", "一条诊断 %d", 42)
	SetSession("unit_session", map[string]any{"k": "v"})
	b, err := json.Marshal(Snap())
	if err != nil {
		t.Fatalf("快照必须可 JSON 序列化: %v", err)
	}
	if !strings.Contains(string(b), "一条诊断 42") {
		t.Fatal("快照日志环应包含刚写入的条目")
	}
}

// TestWatchdogTrips 验证心跳超期触发堆栈转储（短阈值，快速收敛）。
func TestWatchdogTrips(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	Mark("wd-test-stall")
	StartWatchdog(ctx, 20*time.Millisecond, func(name string) time.Duration {
		if name == "wd-test-stall" {
			return 30 * time.Millisecond
		}
		return 0
	})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range logRing.all() {
			if e.Tag == "stack" {
				return // 已转储
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("看门狗未在超期后转储堆栈")
}

// TestWatchdogSilentWhileHealthy 心跳持续刷新时看门狗不得误报。
func TestWatchdogSilentWhileHealthy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	Mark("wd-test-ok")
	StartWatchdog(ctx, 20*time.Millisecond, func(name string) time.Duration {
		if name == "wd-test-ok" {
			return 50 * time.Millisecond
		}
		return 0
	})
	stop := time.After(400 * time.Millisecond)
	for {
		Mark("wd-test-ok")
		select {
		case <-stop:
			// 只看该心跳自身的"判定卡死"事件，避免受前一个用例转储的堆栈干扰
			for _, e := range logRing.all() {
				if e.Tag == "watchdog" && strings.Contains(e.Msg, "wd-test-ok") &&
					strings.Contains(e.Msg, "判定卡死") {
					t.Fatal("心跳健康时不应转储堆栈")
				}
			}
			return
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}
