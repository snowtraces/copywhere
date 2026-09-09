// Package kvm 实现鼠标/键盘跨屏（软 KVM）：主控机捕获本地输入流式转发，
// 副机通过 SendInput 注入。复用 copywhere 的节点发现与 token 鉴权。
//
// 切换模型：光标推向某显示器"暴露边缘"（该侧没有相邻的本地显示器）并继续
// 推动，即切换到该方向配置的邻居；在邻居屏幕上把光标推回共享边缘（穿越其
// 本机多屏布局到达暴露边缘）即释放，控制权回到本机。
//
// 多显示器：被控端入口固定为其主显示器（primary）的共享边缘；随后以相对
// 位移注入，副机本地多屏由操作系统自然跨越。
package kvm

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net"
	"strings"
	"sync"
	"time"

	"copywhere/internal/discovery"
	"copywhere/internal/input"
)

const (
	moveSendInterval = 20 * time.Millisecond // 移动事件合拍间隔（≤50Hz）
	edgePushPixels   = 16                    // 本地边缘推动判定：累计推动像素
	pushGapReset     = 600 * time.Millisecond
	edgeAdjacency    = 2  // 判断两显示器相邻的坐标容差（像素）
	edgeBand         = 2  // 光标距边缘多少像素内算"贴边"
	armPixels        = 30 // 进入副机后深入多少像素才允许触发切回
	leavePushPixels  = 16 // 切回判定：贴边累计推动像素
	pushGapLeave     = 600 * time.Millisecond
	entryMargin      = 2 // 入口位置距共享边缘的像素
	pingInterval     = 1 * time.Second
	readTimeout      = 5 * time.Second
	unconfigLogEvery = 5 * time.Second // 未配置邻居提示的节流间隔
)

// Config 是 KVM 的配置（来自 config.json）。
type Config struct {
	Port         int    // 监听端口（默认 47832）
	Left         string // 光标从本机暴露的左边缘离开时控制的节点名
	Right        string // 光标从本机暴露的右边缘离开时控制的节点名
	EntryMonitor int    // 被控入口显示器索引（-1=主显示器，默认）
}

// Injector 是输入注入接口（生产环境为 input.DefaultInjector，测试用假实现）。
type Injector interface {
	MoveAbs(x, y int)
	MoveRel(dx, dy int)
	Button(down bool, button int)
	Wheel(delta int32, horizontal bool)
	Key(vk, scan uint32, down, ext bool)
	ScreenBounds() (x, y, w, h int)
	CursorPos() (x, y int)
	Monitors() []input.Rect
}

// Callbacks 是输入捕获回调集合（实现 input.Callbacks）。
type Callbacks interface {
	OnMouseMove(dx, dy int)
	OnMouseButton(down bool, button int, x, y int)
	OnWheel(delta int32, horizontal bool)
	OnKey(vk, scan uint32, down, ext bool)
}

type wireMsg struct {
	T     string  `json:"t"`
	Token string  `json:"token,omitempty"`
	Name  string  `json:"name,omitempty"`
	Dir   string  `json:"dir,omitempty"`
	Y     float64 `json:"y,omitempty"` // 主控机光标的垂直比例（用于入口位置）
	DX    int     `json:"dx,omitempty"`
	DY    int     `json:"dy,omitempty"`
	B     int     `json:"b,omitempty"`
	D     int     `json:"d,omitempty"`
	Down  bool    `json:"down,omitempty"`
	H     bool    `json:"h,omitempty"`
	VK    uint32  `json:"vk,omitempty"`
	Scan  uint32  `json:"scan,omitempty"`
	Ext   bool    `json:"ext,omitempty"`
	Msg   string  `json:"msg,omitempty"`
}

type masterSession struct {
	conn     net.Conn
	w        *bufio.Writer
	sendCh   chan wireMsg
	virtX    float64 // 主控机虚拟桌面内未裁剪光标位置（像素）
	virtY    float64
	lastX    float64 // 上次发送时的位置（差值基准）
	lastY    float64
	lastSent time.Time
	stop     chan struct{}
	stopOnce sync.Once
}

type slaveSession struct {
	conn       net.Conn
	w          *bufio.Writer
	dir        string     // "right": 主控机在我左侧；"left": 主控机在我右侧
	entry      input.Rect // 入口显示器
	entryX     int        // 入口放置的光标 X（用于计算深入距离）
	entryTime  time.Time
	armed      bool
	maxDist    int // 距入口位置的最大深入距离
	pushAccum  int // 贴边向外推动的累计像素
	pushLast   time.Time
	leaveEdges []input.Rect // 主控方向上暴露的边缘（切回触发区）
	endOnce    sync.Once
}

// end 结束被控会话（幂等）：可选发送 leave 并关闭连接。
func (sess *slaveSession) end(writeLeave bool, reason string) {
	sess.endOnce.Do(func() {
		if writeLeave {
			sess.conn.SetWriteDeadline(time.Now().Add(time.Second))
			writeMsg(sess.w, wireMsg{T: "leave"})
		}
		sess.conn.Close()
		log.Printf("KVM：被控会话结束（%s）", reason)
	})
}

// Service 是 KVM 服务。
type Service struct {
	cfg      Config
	nodeName string
	token    string
	store    *discovery.Store
	ttl      time.Duration
	injector Injector

	mu               sync.Mutex
	master           *masterSession // 正在控制别人
	serving          *slaveSession  // 正在被控制
	cooldownUntil    time.Time      // 切换失败/释放后的冷却
	attemptUntil     time.Time      // 正在尝试切换（防重复触发）
	pushDir          string
	pushAccum        int
	pushLast         time.Time
	modsDown         map[uint32]bool // 控制期间跟踪的修饰键（归一化后的）状态
	unconfigLogUntil time.Time       // 未配置邻居提示的节流
	firstMoveLogged  bool            // 首次收到鼠标原始输入时打一条连通日志

	// 显示器布局缓存：由后台协程定期刷新。钩子线程只读缓存，
	// 绝不能在持有 s.mu 时做可能阻塞的事情（曾因此死锁拖垮全系统鼠标）。
	monsMu sync.Mutex
	mons   []input.Rect
}

// NewService 创建 KVM 服务。
func NewService(cfg Config, nodeName, token string, store *discovery.Store,
	ttl time.Duration, injector Injector) *Service {
	if cfg.Port <= 0 {
		cfg.Port = 47832
	}
	return &Service{
		cfg: cfg, nodeName: nodeName, token: token,
		store: store, ttl: ttl, injector: injector,
		modsDown: map[uint32]bool{},
	}
}

// inputSetSuppress 包一层避免各处直接依赖 input 包细节。
func inputSetSuppress(b bool) { input.SetSuppress(b) }

// Start 启动被控端监听，直到 ctx 结束。返回错误表示端口被占用等启动失败。
func (s *Service) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp4", fmt.Sprintf("0.0.0.0:%d", s.cfg.Port))
	if err != nil {
		return fmt.Errorf("监听 KVM 端口 %d/tcp 失败: %w", s.cfg.Port, err)
	}
	log.Printf("KVM 被控端已监听 :%d/tcp", s.cfg.Port)
	s.refreshMonitors()
	if mons := s.currentMonitors(); len(mons) > 0 {
		parts := make([]string, len(mons))
		for i, m := range mons {
			p := ""
			if m.Primary {
				p = " 主屏"
			}
			parts[i] = fmt.Sprintf("(%d,%d %dx%d%s)", m.X, m.Y, m.W, m.H, p)
		}
		log.Printf("KVM 显示器布局: %s", strings.Join(parts, " "))
	}
	go func() {
		t := time.NewTicker(3 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.refreshMonitors()
			}
		}
	}()
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	go s.acceptLoop(ctx, ln)
	return nil
}

func (s *Service) acceptLoop(ctx context.Context, ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// 瞬时错误（如对端在 accept 前重置连接）绝不能终止监听，
			// 否则监听 socket 残留、连接能建立却无人应答（表现为握手超时）
			log.Printf("KVM accept 暂时失败: %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go s.handleSlaveConn(conn)
	}
}

// ---------- 本机显示器布局 ----------

// currentMonitors 返回本机显示器布局缓存（由后台协程刷新，本函数不阻塞）。
func (s *Service) currentMonitors() []input.Rect {
	s.monsMu.Lock()
	defer s.monsMu.Unlock()
	if s.mons == nil {
		return []input.Rect{{X: 0, Y: 0, W: 1920, H: 1080, Primary: true}}
	}
	return s.mons
}

func (s *Service) refreshMonitors() {
	if m := s.injector.Monitors(); len(m) > 0 {
		s.monsMu.Lock()
		s.mons = m
		s.monsMu.Unlock()
	}
}

// exposedEdges 计算暴露边缘：某显示器某侧没有（垂直方向有重叠的）相邻显示器
// 贴着，光标才能在该侧推出去。
func exposedEdges(mons []input.Rect) (leftExp, rightExp []input.Rect) {
	for i, m := range mons {
		left, right := true, true
		for j, o := range mons {
			if i == j || !verticalOverlap(m, o) {
				continue
			}
			if abs(o.X+o.W-m.X) <= edgeAdjacency {
				left = false
			}
			if abs(m.X+m.W-o.X) <= edgeAdjacency {
				right = false
			}
		}
		if left {
			leftExp = append(leftExp, m)
		}
		if right {
			rightExp = append(rightExp, m)
		}
	}
	return
}

func verticalOverlap(a, b input.Rect) bool {
	return a.Y < b.Y+b.H-edgeAdjacency && b.Y < a.Y+a.H-edgeAdjacency
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// pickEntryMonitor 返回被控入口显示器：
// entryIdx >= 0 时使用配置指定的显示器（Monitors() 列表下标）；
// 否则恒为主显示器（primary）——用户主工作屏，位置可预期。
func pickEntryMonitor(mons []input.Rect, entryIdx int) input.Rect {
	if entryIdx >= 0 && entryIdx < len(mons) {
		return mons[entryIdx]
	}
	for _, m := range mons {
		if m.Primary {
			return m
		}
	}
	if len(mons) > 0 {
		return mons[0]
	}
	return input.Rect{X: 0, Y: 0, W: 1920, H: 1080, Primary: true}
}

// leaveEdgesFor 返回切回触发边缘：主控方向上暴露的显示器边缘集合。
func leaveEdgesFor(mons []input.Rect, dir string) []input.Rect {
	left, right := exposedEdges(mons)
	if dir == "right" {
		return left
	}
	return right
}

// ---------- 被控端（slave） ----------

func (s *Service) handleSlaveConn(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(readTimeout))
	br := bufio.NewReaderSize(conn, 16*1024)
	bw := bufio.NewWriter(conn)

	var hello wireMsg
	if err := readMsg(br, &hello); err != nil || hello.T != "hello" {
		return
	}
	if subtle.ConstantTimeCompare([]byte(hello.Token), []byte(s.token)) != 1 {
		log.Printf("KVM 拒绝来自 %s 的连接（token 不一致）", conn.RemoteAddr())
		writeMsg(bw, wireMsg{T: "error", Msg: "unauthorized"})
		return
	}
	s.mu.Lock()
	if s.serving != nil || s.master != nil {
		s.mu.Unlock()
		writeMsg(bw, wireMsg{T: "error", Msg: "busy"})
		return
	}
	sess := &slaveSession{conn: conn, w: bw}
	s.serving = sess
	s.mu.Unlock()
	writeMsg(bw, wireMsg{T: "ok"})
	log.Printf("KVM：开始被 %q 控制", hello.Name)
	defer func() {
		s.mu.Lock()
		if s.serving == sess {
			s.serving = nil
		}
		s.mu.Unlock()
	}()

	for {
		conn.SetReadDeadline(time.Now().Add(readTimeout))
		var m wireMsg
		if err := readMsg(br, &m); err != nil {
			if isTimeout(err) { // 主控机失联，自动释放
				sess.end(true, "主控机 "+hello.Name+" 无响应")
			}
			return
		}
		switch m.T {
		case "enter":
			s.mu.Lock()
			sess.dir = m.Dir
			sess.entryTime = time.Now()
			sess.armed = false
			sess.maxDist = 0
			sess.pushAccum = 0
			s.mu.Unlock()

			mons := s.injector.Monitors()
			sess.entry = pickEntryMonitor(mons, s.cfg.EntryMonitor)
			sess.leaveEdges = leaveEdgesFor(mons, m.Dir)
			// 入口：入口显示器的共享边缘；垂直位置跟随主控机光标离开时的高度
			ex := sess.entry.X + entryMargin
			if m.Dir == "left" {
				ex = sess.entry.X + sess.entry.W - 1 - entryMargin
			}
			ey := sess.entry.Y + int(clamp01(m.Y)*float64(sess.entry.H))
			sess.entryX = ex
			s.injector.MoveAbs(ex, ey)
			log.Printf("KVM：入口显示器 (%d,%d %dx%d 主屏=%v)，光标注入到 (%d,%d)",
				sess.entry.X, sess.entry.Y, sess.entry.W, sess.entry.H,
				sess.entry.Primary, ex, ey)
		case "move":
			if sess.dir == "" {
				continue
			}
			s.injector.MoveRel(m.DX, m.DY)
			if s.detectLeave(sess, m.DX) { // 返回 true 表示触发切回
				sess.end(true, "光标推回共享边缘")
				return
			}
		case "btn":
			s.injector.Button(m.Down, m.B)
		case "wheel":
			s.injector.Wheel(int32(m.D), m.H)
		case "key":
			s.injector.Key(m.VK, m.Scan, m.Down, m.Ext)
		case "ping":
			// 保活：必须回应，否则主控端读超时会误判连接断开
			if err := writeMsg(bw, wireMsg{T: "pong"}); err != nil {
				sess.end(false, "回应 ping 失败: "+err.Error())
				return
			}
		default:
			// 未知消息忽略
		}
	}
}

// detectLeave 检测"光标推回共享边缘"的切回动作。光标为真实位置，
// 副机本机多显示器由操作系统自然跨越，因此只需监测暴露边缘。
func (s *Service) detectLeave(sess *slaveSession, dx int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()

	cx, cy := s.injector.CursorPos()
	// 深入距离武装：光标离开共享边缘足够远后才允许触发切回
	dist := abs(cx - sess.entryX)
	if dist > sess.maxDist {
		sess.maxDist = dist
	}
	if !sess.armed {
		if sess.maxDist >= armPixels {
			sess.armed = true
		}
		return false
	}

	atEdge := false
	for _, r := range sess.leaveEdges {
		inY := cy >= r.Y && cy < r.Y+r.H
		if sess.dir == "right" && inY && cx <= r.X+edgeBand {
			atEdge = true
		}
		if sess.dir == "left" && inY && cx >= r.X+r.W-1-edgeBand {
			atEdge = true
		}
	}
	if !atEdge {
		if now.Sub(sess.pushLast) > pushGapLeave {
			sess.pushAccum = 0
		}
		sess.pushLast = now
		return false
	}
	outward := 0
	if sess.dir == "right" && dx < 0 {
		outward = -dx
	}
	if sess.dir == "left" && dx > 0 {
		outward = dx
	}
	sess.pushAccum += outward
	sess.pushLast = now
	return sess.pushAccum >= leavePushPixels
}

// ---------- 主控端（master） ----------

// trySwitch 尝试把控制权切到 dir 方向的邻居（"left"/"right"，按节点名匹配）。
func (s *Service) trySwitch(dir string) {
	var peer *discovery.Peer
	for _, p := range s.store.Alive(s.ttl) {
		if (dir == "right" && p.Name == s.cfg.Right) ||
			(dir == "left" && p.Name == s.cfg.Left) {
			pp := p
			peer = &pp
			break
		}
	}
	if peer == nil {
		log.Printf("KVM：%s 方向的邻居 %q 不在线", dir,
			map[bool]string{true: s.cfg.Right, false: s.cfg.Left}[dir == "right"])
		s.setCooldown(1500 * time.Millisecond)
		return
	}

	var respErr error
	var conn net.Conn
	for _, ip := range peer.IPs {
		c, err := net.DialTimeout("tcp4", fmt.Sprintf("%s:%d", ip, s.cfg.Port), 3*time.Second)
		if err == nil {
			conn = c
			break
		}
		respErr = err
	}
	if conn == nil {
		log.Printf("KVM：连接 %q 失败: %v", peer.Name, respErr)
		s.setCooldown(1500 * time.Millisecond)
		return
	}

	sess, err := s.masterHandshake(conn)
	if err != nil {
		conn.Close()
		log.Printf("KVM：与 %q 握手失败: %v", peer.Name, err)
		s.setCooldown(1500 * time.Millisecond)
		return
	}

	s.mu.Lock()
	s.master = sess
	s.mu.Unlock()
	inputSetSuppress(true)
	cx, cy := s.injector.CursorPos()
	sess.virtX = float64(cx)
	sess.virtY = float64(cy)
	sess.lastX, sess.lastY = sess.virtX, sess.virtY
	// 入口垂直比例：主控机光标在自身虚拟桌面中的高度，副机按此映射
	_, _, _, mvh := s.injector.ScreenBounds()
	ny := 0.5
	if mvh > 0 {
		ny = clamp01(sess.virtY / float64(mvh))
	}
	select {
	case sess.sendCh <- wireMsg{T: "enter", Dir: dir, Y: ny}:
	default:
	}
	log.Printf("KVM：开始控制 %q（向%s），Ctrl+Alt+Shift+X 紧急退出", peer.Name, dir)

	go s.masterSender(sess)
	go s.masterReader(sess, peer.Name)
	go s.masterPinger(sess)
}

func (s *Service) masterHandshake(conn net.Conn) (*masterSession, error) {
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReaderSize(conn, 16*1024)
	bw := bufio.NewWriter(conn)
	if err := writeMsg(bw, wireMsg{T: "hello", Token: s.token, Name: s.nodeName}); err != nil {
		return nil, err
	}
	var resp wireMsg
	if err := readMsg(br, &resp); err != nil {
		return nil, err
	}
	if resp.T != "ok" {
		return nil, fmt.Errorf("%s", resp.Msg)
	}
	sess := &masterSession{
		conn:   conn,
		w:      bw,
		sendCh: make(chan wireMsg, 512),
		stop:   make(chan struct{}),
	}
	conn.SetDeadline(time.Time{}) // 清除握手超时，后续由 ping/读超时保活
	return sess, nil
}

// masterSender 串行写出事件。
func (s *Service) masterSender(sess *masterSession) {
	for {
		select {
		case <-sess.stop:
			return
		case m := <-sess.sendCh:
			if err := writeMsg(sess.w, m); err != nil {
				s.endMasterSession(sess, false, "发送失败: "+err.Error())
				return
			}
		}
	}
}

// masterReader 等待被控端的 leave/error 或连接断开。
func (s *Service) masterReader(sess *masterSession, peerName string) {
	br := bufio.NewReaderSize(sess.conn, 16*1024)
	for {
		sess.conn.SetReadDeadline(time.Now().Add(3 * readTimeout))
		var m wireMsg
		if err := readMsg(br, &m); err != nil {
			s.endMasterSession(sess, false, "与 "+peerName+" 的连接断开")
			return
		}
		switch m.T {
		case "leave":
			s.endMasterSession(sess, false, peerName+" 交还控制权")
			return
		case "error":
			s.endMasterSession(sess, false, peerName+" 返回错误: "+m.Msg)
			return
		case "pong":
			// 被控端对 ping 的应答，读超时随之刷新
		}
	}
}

func (s *Service) masterPinger(sess *masterSession) {
	t := time.NewTicker(pingInterval)
	defer t.Stop()
	for {
		select {
		case <-sess.stop:
			return
		case <-t.C:
			select {
			case sess.sendCh <- wireMsg{T: "ping"}:
			default:
			}
		}
	}
}

// endMasterSession 结束主控会话（幂等）。sendLeave 表示尽力通知对端。
func (s *Service) endMasterSession(sess *masterSession, sendLeave bool, reason string) {
	sess.stopOnce.Do(func() {
		close(sess.stop)
		inputSetSuppress(false)
		if sendLeave {
			sess.conn.SetWriteDeadline(time.Now().Add(time.Second))
			writeMsg(sess.w, wireMsg{T: "leave"})
		}
		sess.conn.Close()
		s.mu.Lock()
		if s.master == sess {
			s.master = nil
		}
		s.mu.Unlock()
		s.setCooldown(1500 * time.Millisecond)
		log.Printf("KVM：控制结束（%s）", reason)
	})
}

// abortMaster 紧急退出（Ctrl+Alt+Shift+X），可在钩子线程调用。
func (s *Service) abortMaster() {
	s.mu.Lock()
	sess := s.master
	s.mu.Unlock()
	if sess != nil {
		go s.endMasterSession(sess, true, "紧急退出热键")
	}
}

func (s *Service) setCooldown(d time.Duration) {
	s.mu.Lock()
	s.cooldownUntil = time.Now().Add(d)
	s.mu.Unlock()
}

// ---------- 输入回调（钩子线程调用，必须非阻塞） ----------

// OnMouseMove 实现 input.Callbacks。
func (s *Service) OnMouseMove(dx, dy int) {
	defer func() { _ = recover() }() // 钩子线程绝不能 panic（会带崩整个进程）
	s.mu.Lock()
	if !s.firstMoveLogged {
		s.firstMoveLogged = true
		log.Printf("KVM：鼠标原始输入链路已连通")
	}
	if m := s.master; m != nil {
		m.virtX += float64(dx)
		m.virtY += float64(dy)
		now := time.Now()
		if now.Sub(m.lastSent) < moveSendInterval {
			s.mu.Unlock()
			return
		}
		m.lastSent = now
		// 发送自上次发送以来的累计位移（未裁剪，副机自行处理边界）
		ddx := int(math.Round(m.virtX - m.lastX))
		ddy := int(math.Round(m.virtY - m.lastY))
		m.lastX += float64(ddx)
		m.lastY += float64(ddy)
		msg := wireMsg{T: "move", DX: ddx, DY: ddy}
		select {
		case m.sendCh <- msg:
		default: // 队列满则丢弃移动事件，位移由后续事件补足
		}
		s.mu.Unlock()
		return
	}
	if s.serving != nil || time.Now().Before(s.attemptUntil) {
		s.mu.Unlock()
		return
	}

	// 本地模式：暴露边缘推动检测（原始位移 + 真实光标位置）
	now := time.Now()
	x, y := s.injector.CursorPos()
	leftExp, rightExp := exposedEdges(s.currentMonitors())
	dir, push := "", 0
	if dx > 0 {
		for _, r := range rightExp {
			if x >= r.X+r.W-1-edgeBand && y >= r.Y && y < r.Y+r.H {
				dir, push = "right", dx
				break
			}
		}
	}
	if dir == "" && dx < 0 {
		for _, r := range leftExp {
			if x <= r.X+edgeBand && y >= r.Y && y < r.Y+r.H {
				dir, push = "left", -dx
				break
			}
		}
	}
	if dir == "" {
		// 反向移动立即清零累积（防误触）；离开边缘由超时规则清理
		if s.pushAccum > 0 {
			if (s.pushDir == "right" && dx < 0) || (s.pushDir == "left" && dx > 0) {
				s.pushDir, s.pushAccum = "", 0
			}
		}
		if s.pushAccum > 0 && now.Sub(s.pushLast) > pushGapReset {
			s.pushDir, s.pushAccum = "", 0
		}
		s.pushLast = now
		s.mu.Unlock()
		return
	}
	if dir != s.pushDir {
		s.pushDir = dir
		s.pushAccum = 0
	}
	s.pushAccum += push
	s.pushLast = now
	if s.pushAccum < edgePushPixels {
		s.mu.Unlock()
		return
	}

	neighbor := s.cfg.Right
	if dir == "left" {
		neighbor = s.cfg.Left
	}
	s.pushAccum = 0
	s.pushDir = ""
	if neighbor == "" {
		// 节流提示：向未配置邻居的方向推动，避免"没反应"无从排查
		if now.After(s.unconfigLogUntil) {
			s.unconfigLogUntil = now.Add(unconfigLogEvery)
			s.mu.Unlock()
			log.Printf("KVM：%s 方向未配置邻居（kvm_%s），忽略切换", dir, dir)
			return
		}
		s.mu.Unlock()
		return
	}
	s.attemptUntil = now.Add(3 * time.Second)
	s.mu.Unlock()
	go s.trySwitch(dir)
}

// OnMouseButton 实现 input.Callbacks。
func (s *Service) OnMouseButton(down bool, button int, x, y int) {
	defer func() { _ = recover() }()
	s.mu.Lock()
	m := s.master
	s.mu.Unlock()
	if m == nil {
		return
	}
	select {
	case m.sendCh <- wireMsg{T: "btn", B: button, Down: down}:
	default:
	}
}

// OnWheel 实现 input.Callbacks。
func (s *Service) OnWheel(delta int32, horizontal bool) {
	defer func() { _ = recover() }()
	s.mu.Lock()
	m := s.master
	s.mu.Unlock()
	if m == nil {
		return
	}
	select {
	case m.sendCh <- wireMsg{T: "wheel", D: int(delta), H: horizontal}:
	default:
	}
}

// OnKey 实现 input.Callbacks。控制期间转发按键并监视紧急退出热键。
func (s *Service) OnKey(vk, scan uint32, down, ext bool) {
	defer func() { _ = recover() }()
	s.mu.Lock()
	m := s.master
	s.mu.Unlock()
	if m == nil {
		return
	}
	// 紧急退出：Ctrl+Alt+Shift+X（按下 X 时判定）
	if down && vk == 0x58 && s.modsAllDown() {
		s.abortMaster()
		return
	}
	s.trackMod(vk, down)
	select {
	case m.sendCh <- wireMsg{T: "key", VK: vk, Scan: scan, Down: down, Ext: ext}:
	default:
	}
}

// OnLocalActivity 实现 input.Callbacks：被控期间检测到本机物理输入（鼠标/键盘），
// 立即结束被控会话——真人已回到本机操作。
func (s *Service) OnLocalActivity() {
	defer func() { _ = recover() }()
	s.mu.Lock()
	sess := s.serving
	s.mu.Unlock()
	if sess == nil {
		return
	}
	go sess.end(true, "本机有物理输入")
}

// 修饰键状态跟踪（控制期间本地键被吞掉，GetAsyncKeyState 不可靠）。
var modKeys = map[uint32]uint32{ // 子键 → 归一化
	0x10: 0x10, 0xA0: 0x10, 0xA1: 0x10, // shift
	0x11: 0x11, 0xA2: 0x11, 0xA3: 0x11, // ctrl
	0x12: 0x12, 0xA4: 0x12, 0xA5: 0x12, // alt
}

func (s *Service) trackMod(vk uint32, down bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if base, ok := modKeys[vk]; ok {
		if down {
			s.modsDown[base] = true
		} else {
			delete(s.modsDown, base)
		}
	}
}

func (s *Service) modsAllDown() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.modsDown[0x10] && s.modsDown[0x11] && s.modsDown[0x12]
}

// ---------- 工具 ----------

func readMsg(br *bufio.Reader, m *wireMsg) error {
	line, err := br.ReadSlice('\n')
	if err != nil {
		return err
	}
	return json.Unmarshal(trimEOL(line), m)
}

func writeMsg(bw *bufio.Writer, m wireMsg) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if _, err := bw.Write(append(b, '\n')); err != nil {
		return err
	}
	return bw.Flush()
}

func trimEOL(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

func isTimeout(err error) bool {
	ne, ok := err.(net.Error)
	return ok && ne.Timeout()
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
