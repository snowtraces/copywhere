// Package diag 提供进程内运行时诊断（借鉴 MouseWithoutBorders 的
// GdiPlusDiag / 环形缓冲日志思路，落地为无外部依赖的轻量实现）：
//
//   - 有界环形日志：诊断事件写入固定容量的内存环，永不撑爆内存；
//   - 原子计数器：包数/字节数/事件数等热点统计只做一次原子自增，
//     在传输与钩子路径上的开销可忽略，绝不引入锁竞争；
//   - 心跳与看门狗：关键循环周期性 Mark()，后台巡检超时即判定卡死，
//     自动转储全部 goroutine 堆栈到环形日志——这是排查"程序还活着
//     但不干活"这类问题最有效的手段；
//   - 快照：Snapshot() 汇总计数器、心跳年龄、活动会话，供面板
//     /api/debug 与命令行展示。
//
// 设计约束：本包不得成为任何功能路径的依赖——所有入口函数在
// 未初始化（零值可用）状态下也必须安全，调用方无需判空。
package diag

import (
	"context"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------- 环形日志 ----------

const ringCap = 512

type entry struct {
	At  time.Time `json:"at"`
	Tag string    `json:"tag"`
	Msg string    `json:"msg"`
}

type ring struct {
	mu      sync.Mutex
	buf     [ringCap]entry
	next, n int
}

var logRing ring

func (r *ring) add(tag, msg string) {
	r.mu.Lock()
	r.buf[r.next] = entry{At: time.Now(), Tag: tag, Msg: msg}
	r.next = (r.next + 1) % ringCap
	if r.n < ringCap {
		r.n++
	}
	r.mu.Unlock()
}

func (r *ring) all() []entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]entry, 0, r.n)
	start := (r.next - r.n + ringCap*2) % ringCap
	for i := 0; i < r.n; i++ {
		out = append(out, r.buf[(start+i)%ringCap])
	}
	return out
}

// Logf 记录一条诊断事件（tag 标注子系统，如 transport/kvm/clip）。
func Logf(tag, format string, a ...any) {
	logRing.add(tag, fmt.Sprintf(format, a...))
}

// ---------- 原子计数器 ----------

var (
	countersMu sync.Mutex
	counters   = map[string]*atomic.Int64{}
)

// Add 对命名计数器原子增量（delta 可为负，用于活动会话数等）。
func Add(name string, delta int64) {
	countersMu.Lock()
	c, ok := counters[name]
	if !ok {
		c = &atomic.Int64{}
		counters[name] = c
	}
	countersMu.Unlock()
	c.Add(delta)
}

// Incr 计数器加一。
func Incr(name string) { Add(name, 1) }

// Counter 返回计数器当前值（不存在时为 0）。
func Counter(name string) int64 {
	countersMu.Lock()
	c := counters[name]
	countersMu.Unlock()
	if c == nil {
		return 0
	}
	return c.Load()
}

func allCounters() map[string]int64 {
	countersMu.Lock()
	defer countersMu.Unlock()
	out := make(map[string]int64, len(counters))
	for k, v := range counters {
		out[k] = v.Load()
	}
	return out
}

// ---------- 心跳与看门狗 ----------

type beat struct {
	name string
	last atomic.Int64 // unix nano
}

var (
	heartMu sync.Mutex
	hearts  = map[string]*beat{}
)

// Mark 记录一次心跳；关键循环每次迭代调用即可。
func Mark(name string) {
	heartMu.Lock()
	b, ok := hearts[name]
	if !ok {
		b = &beat{name: name}
		hearts[name] = b
	}
	heartMu.Unlock()
	b.last.Store(time.Now().UnixNano())
}

// Heartbeat 返回指定心跳距上次 Mark 的间隔；从未 Mark 过返回 0。
func Heartbeat(name string) time.Duration {
	heartMu.Lock()
	b := hearts[name]
	heartMu.Unlock()
	if b == nil {
		return 0
	}
	if v := b.last.Load(); v > 0 {
		return time.Since(time.Unix(0, v))
	}
	return 0
}

// StartWatchdog 启动看门狗巡检：每 check 周期检查一次，任一已注册心跳
// 超过其预算时间未更新即判定卡死，转储全部 goroutine 堆栈到环形日志。
// 同一子系统只转储一次（直到它恢复心跳后再次超期），避免堆栈刷屏。
//
// budget 返回某心跳允许的静默上限：返回 0 表示该项不监管（如未启用的
// 子系统），-1 以外的正值即超时阈值。
func StartWatchdog(ctx context.Context, check time.Duration, budget func(name string) time.Duration) {
	go func() {
		t := time.NewTicker(check)
		defer t.Stop()
		tripped := map[string]bool{}
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			heartMu.Lock()
			names := make([]string, 0, len(hearts))
			for n := range hearts {
				names = append(names, n)
			}
			heartMu.Unlock()
			for _, n := range names {
				lim := budget(n)
				if lim <= 0 {
					continue
				}
				age := Heartbeat(n)
				if age == 0 || age < lim {
					tripped[n] = false // 已恢复，允许下次再触发
					continue
				}
				if tripped[n] {
					continue
				}
				tripped[n] = true
				Logf("watchdog", "心跳 %q 已 %s 未更新（阈值 %s），判定卡死，转储 goroutine 堆栈",
					n, age.Round(time.Second), lim)
				DumpStacks()
			}
		}
	}()
}

// DumpStacks 把全部 goroutine 堆栈转储进环形日志（按空行分条，保留函数名）。
func DumpStacks() int {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	text := strings.ReplaceAll(string(buf[:n]), "\r\n", "\n")
	blocks := strings.Split(strings.TrimSpace(text), "\n\n")
	count := 0
	for _, b := range blocks {
		b = strings.TrimSpace(b)
		if b == "" {
			continue
		}
		one := strings.ReplaceAll(b, "\n", " | ")
		if len(one) > 1200 {
			one = one[:1200] + " …(截断)"
		}
		logRing.add("stack", one)
		count++
	}
	Logf("watchdog", "堆栈转储完成：%d 个 goroutine", count)
	return count
}

// ---------- 活动会话（附加信息，供快照展示） ----------

type info struct {
	mu    sync.Mutex
	items map[string]any
}

var sessionInfo = &info{items: map[string]any{}}

func (i *info) set(key string, v any) {
	i.mu.Lock()
	i.items[key] = v
	i.mu.Unlock()
}

func (i *info) get() map[string]any {
	i.mu.Lock()
	defer i.mu.Unlock()
	out := make(map[string]any, len(i.items))
	for k, v := range i.items {
		out[k] = v
	}
	return out
}

// SetSession 登记一个子系统当前活动状态（如 KVM 会话、监听窗口），
// 值须为可 JSON 序列化的结构。传 nil 表示清除。
func SetSession(key string, v any) { sessionInfo.set(key, v) }

// ---------- 快照 ----------

// Snapshot 是诊断快照（供 /api/debug 序列化输出）。
type Snapshot struct {
	Counters   map[string]int64  `json:"counters"`
	Heartbeats map[string]string `json:"heartbeats"` // 距上次更新的时长
	Sessions   map[string]any    `json:"sessions"`
	Goroutines int               `json:"goroutines"`
	MemHeapMB  float64           `json:"mem_heap_mb"`
	Uptime     string            `json:"uptime"`
	Log        []entry           `json:"log"`
}

var startTime = time.Now()

// Snap 采集一次完整快照。
func Snap() Snapshot {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	hb := map[string]string{}
	heartMu.Lock()
	for n, b := range hearts {
		if v := b.last.Load(); v > 0 {
			hb[n] = time.Since(time.Unix(0, v)).Round(100 * time.Millisecond).String()
		} else {
			hb[n] = "从未"
		}
	}
	heartMu.Unlock()
	logs := logRing.all()
	if len(logs) > 128 {
		logs = logs[len(logs)-128:] // 响应体瘦身，面板只展示尾部
	}
	return Snapshot{
		Counters:   allCounters(),
		Heartbeats: hb,
		Sessions:   sessionInfo.get(),
		Goroutines: runtime.NumGoroutine(),
		MemHeapMB:  float64(ms.HeapAlloc) / (1 << 20),
		Uptime:     time.Since(startTime).Round(time.Second).String(),
		Log:        logs,
	}
}

// Text 把快照渲染为便于命令行/日志阅读的文本。
func (s Snapshot) Text() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "运行时长 %s，goroutine %d，堆内存 %.1f MB\n", s.Uptime, s.Goroutines, s.MemHeapMB)
	sb.WriteString("计数器：\n")
	names := make([]string, 0, len(s.Counters))
	for k := range s.Counters {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		fmt.Fprintf(&sb, "  %-28s %d\n", k, s.Counters[k])
	}
	if len(s.Heartbeats) > 0 {
		sb.WriteString("心跳：\n")
		for _, k := range sortedKeys(s.Heartbeats) {
			fmt.Fprintf(&sb, "  %-16s 距上次 %s\n", k, s.Heartbeats[k])
		}
	}
	if len(s.Sessions) > 0 {
		sb.WriteString("活动会话：\n")
		for _, k := range sortedKeysAny(s.Sessions) {
			fmt.Fprintf(&sb, "  %-16s %v\n", k, s.Sessions[k])
		}
	}
	if len(s.Log) > 0 {
		sb.WriteString("诊断日志（尾部）：\n")
		start := 0
		if len(s.Log) > 40 {
			start = len(s.Log) - 40
		}
		for _, e := range s.Log[start:] {
			fmt.Fprintf(&sb, "  %s [%s] %s\n", e.At.Format("15:04:05"), e.Tag, e.Msg)
		}
	}
	return sb.String()
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedKeysAny(m map[string]any) []string { return sortedKeys(m) }
