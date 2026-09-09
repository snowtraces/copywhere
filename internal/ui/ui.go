// Package ui 提供 run 命令的终端界面：日志 / 在线节点 / 文件记录 三个标签页。
package ui

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-runewidth"

	"copywhere/internal/bytesize"
	"copywhere/internal/discovery"
)

const (
	maxLogLines = 1000
	maxFileRecs = 500
	chBuffer    = 256
	tickEvery   = 1500 * time.Millisecond
)

// FileRecord 是一次文件收发记录。
type FileRecord struct {
	Time   time.Time
	In     bool // true=接收，false=发送
	Peer   string
	Name   string
	Size   int64
	Status string
	Detail string
}

// Hub 收集 log 输出（log.SetOutput 的目标）；mirror 非 nil 时同步镜像输出。
type Hub struct {
	mu     sync.Mutex
	lines  []string
	mirror io.Writer
	sub    chan string
}

// NewHub 创建日志枢纽；plain 模式传 os.Stdout 做镜像，TUI 模式传 nil。
func NewHub(mirror io.Writer) *Hub { return &Hub{mirror: mirror} }

// Write 实现 io.Writer，逐行缓存并通知订阅者。
func (h *Hub) Write(p []byte) (int, error) {
	line := strings.TrimRight(string(p), "\r\n")
	h.mu.Lock()
	h.lines = append(h.lines, line)
	if len(h.lines) > maxLogLines {
		h.lines = h.lines[len(h.lines)-maxLogLines:]
	}
	sub := h.sub
	h.mu.Unlock()
	if h.mirror != nil {
		h.mirror.Write(p)
	}
	if sub != nil {
		select {
		case sub <- line:
		default: // 订阅者消费不过来时丢弃，绝不阻塞业务日志
		}
	}
	return len(p), nil
}

// Snapshot 返回当前全部日志行的副本。
func (h *Hub) Snapshot() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.lines))
	copy(out, h.lines)
	return out
}

func (h *Hub) SetSubscriber(ch chan string) {
	h.mu.Lock()
	h.sub = ch
	h.mu.Unlock()
}

// Controller 是 TUI 对外暴露的控制接口，app 通过它投递日志与文件记录。
type Controller struct {
	Hub      *Hub
	peersFn  func() []discovery.Peer
	ttl      time.Duration
	selfName string
	selfIP   string
	fileCh   chan FileRecord
}

// NewController 创建 TUI 控制器；peersFn 用于周期刷新在线节点列表，
// selfName/selfIP 用于在节点页展示 [本机] 行。
func NewController(peersFn func() []discovery.Peer, ttl time.Duration, selfName, selfIP string) *Controller {
	return &Controller{
		Hub:      NewHub(nil),
		peersFn:  peersFn,
		ttl:      ttl,
		selfName: selfName,
		selfIP:   selfIP,
		fileCh:   make(chan FileRecord, chBuffer),
	}
}

// LogWriter 返回可直接交给 log.SetOutput 的写入器。
func (c *Controller) LogWriter() io.Writer { return c.Hub }

// AddFile 投递一条文件记录（非阻塞，缓冲满则丢弃）。
func (c *Controller) AddFile(r FileRecord) {
	select {
	case c.fileCh <- r:
	default:
	}
}

// Run 阻塞运行 TUI，直到用户退出（q/Esc/Ctrl+C）或 ctx 结束。
func (c *Controller) Run(ctx context.Context) error {
	_, err := tea.NewProgram(newModel(c), tea.WithAltScreen(), tea.WithContext(ctx)).Run()
	return err
}

// ---------- bubbletea 模型 ----------

type model struct {
	ctl        *Controller
	tab        int // 0=日志 1=在线节点 2=文件记录
	logs       []string
	files      []FileRecord
	peers      []discovery.Peer
	w, h       int
	logOffset  int // 相对底部的滚动偏移，0=跟随最新
	fileOffset int
	logCh      chan string
}

func newModel(c *Controller) model {
	return model{
		ctl:   c,
		logCh: make(chan string, chBuffer),
		logs:  c.Hub.Snapshot(),
		w:     80, h: 24,
		peers: c.peersFn(),
	}
}

type logMsg string
type fileMsg FileRecord
type tickMsg struct{}

func (m model) Init() tea.Cmd {
	m.ctl.Hub.SetSubscriber(m.logCh)
	return tea.Batch(waitLog(m.logCh), waitFile(m.ctl.fileCh), tick())
}

func waitLog(ch chan string) tea.Cmd {
	return func() tea.Msg {
		s, ok := <-ch
		if !ok {
			return nil
		}
		return logMsg(s)
	}
}

func waitFile(ch chan FileRecord) tea.Cmd {
	return func() tea.Msg {
		r, ok := <-ch
		if !ok {
			return nil
		}
		return fileMsg(r)
	}
}

func tick() tea.Cmd {
	return tea.Tick(tickEvery, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		// 部分终端环境会报告 0 尺寸；此时保留默认值，否则正文会被截成空白
		if msg.Width > 0 {
			m.w = msg.Width
		}
		if msg.Height > 0 {
			m.h = msg.Height
		}
		return m, nil

	case tickMsg:
		if m.ctl.peersFn != nil {
			m.peers = m.ctl.peersFn()
		}
		return m, tick()

	case logMsg:
		m.logs = append(m.logs, string(msg))
		if len(m.logs) > maxLogLines {
			m.logs = m.logs[len(m.logs)-maxLogLines:]
		}
		return m, waitLog(m.logCh)

	case fileMsg:
		m.files = append(m.files, FileRecord(msg))
		if len(m.files) > maxFileRecs {
			m.files = m.files[len(m.files)-maxFileRecs:]
		}
		return m, waitFile(m.ctl.fileCh)

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q", "esc":
			return m, tea.Quit
		case "1":
			m.tab, m.logOffset = 0, 0
		case "2":
			m.tab = 1
		case "3":
			m.tab, m.fileOffset = 2, 0
		case "tab", "right":
			m.tab = (m.tab + 1) % 3
		case "left":
			m.tab = (m.tab + 2) % 3
		case "up", "k":
			m.scroll(-1)
		case "down", "j":
			m.scroll(1)
		case "pgup":
			m.scroll(-(m.contentH() - 1))
		case "pgdown":
			m.scroll(m.contentH() - 1)
		}
		return m, nil
	}
	return m, nil
}

func (m *model) contentH() int {
	h := m.h - 4 // 标签行 + 空行 + 页脚 + 余量
	if h < 1 {
		h = 1
	}
	return h
}

// scroll delta<0 向上翻历史，delta>0 回到最新方向。
func (m *model) scroll(delta int) {
	switch m.tab {
	case 0:
		m.logOffset = clamp(m.logOffset+delta, 0, max(0, len(m.logs)-1))
	case 2:
		m.fileOffset = clamp(m.fileOffset+delta, 0, max(0, len(m.files)-1))
	}
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func (m model) View() string {
	var b strings.Builder

	// 标签栏
	tabs := []string{"日志", "在线节点", "文件记录"}
	for i, t := range tabs {
		label := fmt.Sprintf(" %d %s ", i+1, t)
		if i == m.tab {
			b.WriteString("\x1b[7m" + label + "\x1b[0m")
		} else {
			b.WriteString("\x1b[2m" + label + "\x1b[0m")
		}
		b.WriteString(" ")
	}
	b.WriteString("\n\n")

	h := m.contentH()
	var body []string
	switch m.tab {
	case 0:
		body = m.logView(h)
	case 1:
		body = m.peersView(h)
	case 2:
		body = m.filesView(h)
	}
	for _, line := range body {
		b.WriteString(runewidth.Truncate(line, m.w, "…"))
		b.WriteString("\n")
	}

	// 页脚
	b.WriteString("\n\x1b[2m Tab/←→ 切换 · ↑↓/PgUp/PgDn 滚动 · q 退出 · 在线节点 ")
	b.WriteString(fmt.Sprint(len(m.peers)))
	b.WriteString(" \x1b[0m")
	return b.String()
}

// window 取切片 [start:end) 的最后 h 行，不足在底部补空行（内容顶部对齐）。
func window(all []string, offset, h int) []string {
	end := len(all) - offset
	if end < 0 {
		end = 0
	}
	start := end - h
	if start < 0 {
		start = 0
	}
	vis := all[start:end]
	for len(vis) < h {
		vis = append(vis, "")
	}
	return vis
}

func (m model) logView(h int) []string {
	return window(m.logs, m.logOffset, h)
}

func (m model) peersView(h int) []string {
	// 首行固定显示本机：[本机]名称(ip)
	lines := []string{"[本机]" + m.ctl.selfName + "(" + m.ctl.selfIP + ")"}
	for _, p := range m.peers {
		if len(p.IPs) == 0 {
			lines = append(lines, "[远程]"+p.Name+"(地址未知)")
			continue
		}
		idle := time.Since(p.LastSeen).Round(time.Second)
		for i, ip := range p.IPs {
			if i == 0 {
				// 首选地址与节点名同行，附带端口与活跃信息（正文行不带 ANSI，见 View 截断说明）
				lines = append(lines, fmt.Sprintf("[远程]%s(%s) · %d/tcp · %s 前",
					p.Name, ip, p.TCPPort, idle))
			} else {
				// 其余已知地址逐行列出（发送时逐个尝试，任一可达即可）
				lines = append(lines, fmt.Sprintf("      └─ %s (备选地址 %d/%d)",
					ip, i+1, len(p.IPs)))
			}
		}
	}
	if len(m.peers) == 0 {
		lines = append(lines, "", "（暂无在线节点，等待 UDP 广播发现…）")
	}
	return window(lines, 0, h)
}

func (m model) filesView(h int) []string {
	if len(m.files) == 0 {
		return append([]string{"（暂无文件收发记录）"}, blank(h-1)...)
	}
	lines := []string{fmt.Sprintf(" %-9s %-4s %-20s %-30s %9s  %s",
		"时间", "方向", "对端", "文件", "大小", "状态")}
	for i := len(m.files) - 1; i >= 0; i-- {
		r := m.files[i]
		dir := "发送"
		if r.In {
			dir = "接收"
		}
		status := r.Status
		lines = append(lines, fmt.Sprintf(" %-9s %-4s %-20s %-30s %9s  %s",
			r.Time.Format("15:04:05"), dir, fit(r.Peer, 20), fit(r.Name, 30),
			fit(bytesize.Human(r.Size), 9), status))
	}
	return window(lines, m.fileOffset, h)
}

// fit 截断/补齐到指定显示宽度（兼容中日韩宽字符）。
func fit(s string, w int) string {
	if runewidth.StringWidth(s) > w {
		return runewidth.Truncate(s, w-1, "…")
	}
	return s + strings.Repeat(" ", w-runewidth.StringWidth(s))
}

func blank(n int) []string {
	out := make([]string, max(0, n))
	for i := range out {
		out[i] = ""
	}
	return out
}
