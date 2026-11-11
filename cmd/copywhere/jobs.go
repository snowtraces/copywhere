package main

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"copywhere/internal/app"
)

// 任务状态。
const (
	jobRunning = "running"
	jobDone    = "done"
)

// jobPeerResult 是单个对端的投递结果（命令行汇总用）。
type jobPeerResult struct {
	Peer  string `json:"peer"`
	OK    bool   `json:"ok"`
	Retry bool   `json:"retry"` // 失败但会进入重试队列（对端暂不可达）
	Msg   string `json:"msg,omitempty"`
}

// jobView 是可回传给命令行的任务快照。
type jobView struct {
	ID         string          `json:"id"`
	Kind       string          `json:"kind"`  // files | text
	State      string          `json:"state"` // running | done
	StartedAt  string          `json:"startedAt"`
	FinishedAt string          `json:"finishedAt,omitempty"`
	Results    []jobPeerResult `json:"results"`
	Error      string          `json:"error,omitempty"`

	finished time.Time // 内部用于过期清理，不参与 JSON
}

// jobSender 是任务真正执行发送所需的最小能力（*app.App 实现，测试用假实现）。
type jobSender interface {
	SendFilesWithResult(paths []string) []app.SendResult
	SendTextWithResult(text string) []app.SendResult
	RejectedWith(id string) bool
}

const (
	// maxJobs 是保留的最近任务数（CLI 轮询完就不再用，留一点余量便于排查）。
	maxJobs = 32
	// jobRetention 是任务快照的保留时长。
	jobRetention = 10 * time.Minute
)

// jobRegistry 让 GUI 承接命令行的发送请求，并允许命令行按 job id 轮询结果。
//
// 为什么是"异步任务 + 轮询"而不是"管道里同步等"：本机命名管道是单实例串行的，
// 一次几分钟的大文件传输会把右键发送、二次启动、其它 CLI 请求全部堵住；
// 这里让发送在独立 goroutine 里跑，管道只负责毫秒级的建任务/查状态。
type jobRegistry struct {
	mu    sync.Mutex
	send  jobSender
	jobs  map[string]*jobView
	order []string
	seq   atomic.Uint64
}

func newJobRegistry(send jobSender) *jobRegistry {
	return &jobRegistry{send: send, jobs: make(map[string]*jobView)}
}

// startFiles 启动一次文件投送任务，返回任务 id。
func (r *jobRegistry) startFiles(paths []string) (string, error) {
	if len(paths) == 0 {
		return "", errors.New("没有要发送的路径")
	}
	return r.start("files", func() []app.SendResult { return r.send.SendFilesWithResult(paths) }), nil
}

// startText 启动一次文本投送任务，返回任务 id。
func (r *jobRegistry) startText(text string) (string, error) {
	if text == "" {
		return "", errors.New("文本不能为空")
	}
	return r.start("text", func() []app.SendResult { return r.send.SendTextWithResult(text) }), nil
}

// start 创建任务并在后台执行；结果写回任务快照。
func (r *jobRegistry) start(kind string, run func() []app.SendResult) string {
	id := fmt.Sprintf("%x-%d", time.Now().UnixNano(), r.seq.Add(1))
	view := &jobView{
		ID:        id,
		Kind:      kind,
		State:     jobRunning,
		StartedAt: time.Now().Format("15:04:05"),
	}
	r.mu.Lock()
	r.pruneLocked()
	r.jobs[id] = view
	r.order = append(r.order, id)
	r.mu.Unlock()

	go func() {
		res := run()
		results := make([]jobPeerResult, 0, len(res))
		for _, one := range res {
			results = append(results, jobPeerResult{
				Peer:  one.Peer,
				OK:    one.OK,
				Retry: !one.OK && !r.send.RejectedWith(one.ID),
				Msg:   one.Msg,
			})
		}
		r.mu.Lock()
		view.Results = results
		view.State = jobDone
		view.finished = time.Now()
		view.FinishedAt = view.finished.Format("15:04:05")
		r.mu.Unlock()
	}()
	return id
}

// get 返回任务快照；不存在或已过期时返回 false。
func (r *jobRegistry) get(id string) (jobView, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked()
	view, ok := r.jobs[id]
	if !ok {
		return jobView{}, false
	}
	return *view, true
}

// pruneLocked 删除超过保留时长的已完成任务，并把已完成任务数量压到 maxJobs 以内
// （运行中的任务不删，否则正在轮询的命令行会突然拿到"任务不存在"）。
func (r *jobRegistry) pruneLocked() {
	cutoff := time.Now().Add(-jobRetention)
	for id, view := range r.jobs {
		if view.State == jobDone && !view.finished.IsZero() && view.finished.Before(cutoff) {
			delete(r.jobs, id)
		}
	}
	done := 0
	for _, id := range r.order {
		view, ok := r.jobs[id]
		if !ok {
			continue
		}
		if view.State != jobDone {
			continue
		}
		done++
		if done > maxJobs {
			delete(r.jobs, id)
		}
	}
}
