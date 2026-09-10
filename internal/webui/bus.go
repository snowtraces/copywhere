// Package webui 提供 gui 命令的系统托盘与浏览器控制面板：
// 事件总线（Bus）、本地 HTTP 服务（REST + SSE）、托盘图标。
//
// 安全模型：只监听 127.0.0.1，所有请求需携带随机访问 token
// （托盘打开面板时通过 URL 参数带入，服务端随后种下 cookie）。
package webui

import (
	"sync"

	"copywhere/internal/app"
	"copywhere/internal/ui"
)

const (
	maxLogLines = 1000
	maxFileRecs = 500
	chBuffer    = 256
)

// Event 是推送给面板的一条事件；Data 为已定义的 JSON 视图结构。
type Event struct {
	Type string `json:"type"`
	Data any    `json:"data"`
}

// 事件类型常量。
const (
	EvLog      = "log"
	EvFile     = "file"
	EvProgress = "progress"
	EvPeers    = "peers"
	EvState    = "state"
	EvPairReq  = "pair_request"
)

// fileJSON 是 ui.FileRecord 的 JSON 视图。
type fileJSON struct {
	Time   string `json:"time"`
	In     bool   `json:"in"`
	Peer   string `json:"peer"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// Bus 收集运行事件：缓存日志与文件记录供面板补齐历史，
// 并向所有已连接的 SSE 订阅者实时推送。实现 app.EventSink。
type Bus struct {
	mu    sync.Mutex
	logs  []string
	files []ui.FileRecord
	subs  map[chan Event]struct{}
}

// NewBus 创建事件总线。
func NewBus() *Bus {
	return &Bus{subs: map[chan Event]struct{}{}}
}

// Log 实现 app.EventSink：缓存一行日志并推送。
func (b *Bus) Log(line string) {
	b.mu.Lock()
	b.logs = append(b.logs, line)
	if len(b.logs) > maxLogLines {
		b.logs = b.logs[len(b.logs)-maxLogLines:]
	}
	b.mu.Unlock()
	b.publish(Event{Type: EvLog, Data: map[string]string{"line": line}})
}

// AddFile 实现 app.EventSink：缓存一条文件记录并推送。
func (b *Bus) AddFile(r ui.FileRecord) {
	b.mu.Lock()
	b.files = append(b.files, r)
	if len(b.files) > maxFileRecs {
		b.files = b.files[len(b.files)-maxFileRecs:]
	}
	b.mu.Unlock()
	b.publish(Event{Type: EvFile, Data: fileToJSON(r)})
}

// Progress 实现 app.EventSink：推送发送进度（不缓存）。
func (b *Bus) Progress(p app.Progress) {
	b.publish(Event{Type: EvProgress, Data: p})
}

// PairRequest 实现 app.EventSink：推送配对请求（面板弹出确认框）。
func (b *Bus) PairRequest(id, name string) {
	b.publish(Event{Type: EvPairReq, Data: map[string]string{"id": id, "name": name}})
}

// Logs 返回缓存日志的副本。
func (b *Bus) Logs() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.logs))
	copy(out, b.logs)
	return out
}

// Files 返回缓存文件记录的副本（旧在前）。
func (b *Bus) Files() []fileJSON {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]fileJSON, len(b.files))
	for i, r := range b.files {
		out[i] = fileToJSON(r)
	}
	return out
}

// Subscribers 返回当前 SSE 订阅者数量（0 时服务端可跳过周期快照构建）。
func (b *Bus) Subscribers() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

// Subscribe 注册一个订阅者，返回事件通道与取消函数。
func (b *Bus) Subscribe() (chan Event, func()) {
	ch := make(chan Event, chBuffer)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
	}
}

// publish 非阻塞推送；某个订阅者消费不过来时对其丢弃，绝不阻塞业务日志。
func (b *Bus) publish(ev Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.publishLocked(ev, b.subs)
}

// publishTo 非阻塞推送给指定订阅者集合（用于 SSE 建立连接时的定向快照）。
func (b *Bus) publishTo(targets map[chan Event]struct{}, ev Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.publishLocked(ev, targets)
}

func (b *Bus) publishLocked(ev Event, targets map[chan Event]struct{}) {
	for ch := range targets {
		select {
		case ch <- ev:
		default:
		}
	}
}

func fileToJSON(r ui.FileRecord) fileJSON {
	return fileJSON{
		Time:   r.Time.Format("15:04:05"),
		In:     r.In,
		Peer:   r.Peer,
		Name:   r.Name,
		Size:   r.Size,
		Status: r.Status,
		Detail: r.Detail,
	}
}
