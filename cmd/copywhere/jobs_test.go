package main

import (
	"testing"
	"time"

	"copywhere/internal/app"
)

// fakeJobSender 是 jobRegistry 的最小发送端替身。
type fakeJobSender struct {
	results  []app.SendResult
	rejected map[string]bool
	delay    time.Duration
	calls    int
}

func (f *fakeJobSender) SendFilesWithResult(paths []string) []app.SendResult {
	f.calls++
	time.Sleep(f.delay)
	return f.results
}

func (f *fakeJobSender) SendTextWithResult(text string) []app.SendResult {
	f.calls++
	time.Sleep(f.delay)
	return f.results
}

func (f *fakeJobSender) RejectedWith(id string) bool { return f.rejected[id] }

func waitJobDone(t *testing.T, reg *jobRegistry, id string) jobView {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		view, ok := reg.get(id)
		if !ok {
			t.Fatalf("任务 %s 不存在", id)
		}
		if view.State != jobRunning {
			return view
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("任务 %s 在 2s 内未完成", id)
	return jobView{}
}

func TestJobRegistryCollectsPerPeerResults(t *testing.T) {
	sender := &fakeJobSender{
		rejected: map[string]bool{"p3": true},
		results: []app.SendResult{
			{ID: "p1", Peer: "PC-A (10.0.0.2)", OK: true},
			{ID: "p2", Peer: "PC-B (10.0.0.3)", OK: false, Msg: "连接超时"},
			{ID: "p3", Peer: "PC-C (10.0.0.4)", OK: false, Msg: "令牌不一致"},
		},
	}
	reg := newJobRegistry(sender)

	id, err := reg.startFiles([]string{`C:\tmp\a.txt`})
	if err != nil {
		t.Fatalf("startFiles: %v", err)
	}
	view := waitJobDone(t, reg, id)
	if view.Kind != "files" || len(view.Results) != 3 {
		t.Fatalf("任务快照不符: %+v", view)
	}
	// 未成功且未被鉴权拒绝 → 会进重试队列；被拒绝 → 致命失败，不重试
	wantRetry := map[string]bool{"PC-A (10.0.0.2)": false, "PC-B (10.0.0.3)": true, "PC-C (10.0.0.4)": false}
	for _, r := range view.Results {
		if r.Retry != wantRetry[r.Peer] {
			t.Fatalf("%s 的 retry=%v，期望 %v（结果: %+v）", r.Peer, r.Retry, wantRetry[r.Peer], r)
		}
		if !r.OK && r.Msg == "" {
			t.Fatalf("%s 失败但没有原因", r.Peer)
		}
	}
}

func TestJobRegistryTextAndValidation(t *testing.T) {
	sender := &fakeJobSender{results: []app.SendResult{{ID: "p1", Peer: "PC-A", OK: true}}}
	reg := newJobRegistry(sender)

	if _, err := reg.startFiles(nil); err == nil {
		t.Fatal("空路径应报错")
	}
	if _, err := reg.startText(""); err == nil {
		t.Fatal("空文本应报错")
	}
	id, ok := "", false
	if id, ok = mustStart(t, reg, func() (string, error) { return reg.startText("hello") }); !ok {
		t.Fatal("startText 失败")
	}
	view := waitJobDone(t, reg, id)
	if view.Kind != "text" || len(view.Results) != 1 || !view.Results[0].OK {
		t.Fatalf("文本任务快照不符: %+v", view)
	}

	if _, ok := reg.get("no-such-id"); ok {
		t.Fatal("不存在的任务不应返回 ok")
	}
}

func mustStart(t *testing.T, reg *jobRegistry, fn func() (string, error)) (string, bool) {
	t.Helper()
	id, err := fn()
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	return id, true
}

func TestJobRegistryKeepsRunningJobWhenPruning(t *testing.T) {
	sender := &fakeJobSender{delay: 50 * time.Millisecond}
	reg := newJobRegistry(sender)
	id, err := reg.startFiles([]string{"a"})
	if err != nil {
		t.Fatalf("startFiles: %v", err)
	}
	// 塞入远超上限的已完成记录，运行中的任务不能被裁掉
	reg.mu.Lock()
	for i := 0; i < maxJobs*2; i++ {
		finished := time.Now()
		key := "old-" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		reg.jobs[key] = &jobView{ID: key, State: jobDone, finished: finished}
		reg.order = append(reg.order, key)
	}
	reg.mu.Unlock()

	if _, ok := reg.get(id); !ok {
		t.Fatal("运行中的任务被误删")
	}
}
