// Package kvm 实现鼠标/键盘跨屏（软 KVM）：主控机捕获本地输入流式转发，
// 副机通过 SendInput 注入。复用 copywhere 的节点发现与 token 鉴权。
//
// 切换模型：光标推向本机屏幕边缘（配置了邻居的一侧）并继续推动即切换到
// 邻居；在邻居屏幕上向回推过共享边缘即释放，控制权回到本机。
// 坐标协议：主控机发送其虚拟桌面内的归一化坐标 m∈(-∞,∞)，副机按
// dir=right → rel=m-1、dir=left → rel=m+1 映射到自身虚拟桌面（[0,1]）。
package kvm

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"copywhere/internal/discovery"
	"copywhere/internal/input"
)

const (
	moveSendInterval = 20 * time.Millisecond // 移动事件合拍间隔（≤50Hz）
	edgePushPixels   = 24                    // 本地边缘推动判定：累计推动像素
	edgeResetPixels  = 8                     // 离开边缘该距离后重新计数
	pushGapReset     = 400 * time.Millisecond
	leaveGrace       = 300 * time.Millisecond
	leaveSoft        = 0.005 // 归一化越界软阈值（连续 2 次即离开）
	leaveHard        = 0.03  // 归一化越界硬阈值（一次即离开）
	armThreshold     = 0.02  // 进入副机多深后才允许触发离开
	pingInterval     = 1 * time.Second
	readTimeout      = 5 * time.Second
)

// Config 是 KVM 的配置（来自 config.json）。
type Config struct {
	Port  int    // 监听端口（默认 47832）
	Left  string // 光标从本机左边缘离开时控制的节点名
	Right string // 光标从本机右边缘离开时控制的节点名
}

// Injector 是输入注入接口（生产环境为 input.DefaultInjector，测试用假实现）。
type Injector interface {
	MoveAbs(x, y int)
	Button(down bool, button int)
	Wheel(delta int32, horizontal bool)
	Key(vk, scan uint32, down, ext bool)
	ScreenBounds() (x, y, w, h int)
	CursorPos() (x, y int)
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
	Y     float64 `json:"y,omitempty"` // 垂直归一化坐标
	X     float64 `json:"x,omitempty"` // 主控机空间水平归一化坐标
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
	lastSent time.Time
	stop     chan struct{}
	stopOnce sync.Once
}

type slaveSession struct {
	conn        net.Conn
	w           *bufio.Writer
	dir         string // "right": 主控机在我左侧；"left": 主控机在我右侧
	armed       bool
	beyondCount int
	graceUntil  time.Time
}

// Service 是 KVM 服务。
type Service struct {
	cfg      Config
	nodeName string
	token    string
	store    *discovery.Store
	ttl      time.Duration
	injector Injector

	mu            sync.Mutex
	master        *masterSession // 正在控制别人
	serving       *slaveSession  // 正在被控制
	cooldownUntil time.Time      // 切换失败/释放后的冷却
	attemptUntil  time.Time      // 正在尝试切换（防重复触发）
	pushDir       string
	pushAccum     int
	pushLast      time.Time
	modsDown      map[uint32]bool // 控制期间跟踪的修饰键（归一化后的）状态
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
			log.Printf("KVM accept 失败: %v", err)
			return
		}
		go s.handleSlaveConn(conn)
	}
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
				log.Printf("KVM：主控机 %q 无响应，释放控制", hello.Name)
				conn.SetWriteDeadline(time.Now().Add(time.Second))
				writeMsg(bw, wireMsg{T: "leave"})
			}
			return
		}
		switch m.T {
		case "enter":
			s.mu.Lock()
			sess.dir = m.Dir
			sess.graceUntil = time.Now().Add(leaveGrace)
			sess.armed = false
			sess.beyondCount = 0
			s.mu.Unlock()
		case "move":
			if sess.dir == "" {
				continue
			}
			if s.injectMove(sess, m) { // 返回 true 表示触发离开
				writeMsg(bw, wireMsg{T: "leave"})
				log.Printf("KVM：光标推回共享边缘，控制权交还本机")
				return
			}
		case "btn":
			s.injector.Button(m.Down, m.B)
		case "wheel":
			s.injector.Wheel(int32(m.D), m.H)
		case "key":
			s.injector.Key(m.VK, m.Scan, m.Down, m.Ext)
		case "ping":
			// 保活
		default:
			// 未知消息忽略
		}
	}
}

// injectMove 注入绝对移动并检测"推回共享边缘"的离开动作。
func (s *Service) injectMove(sess *slaveSession, m wireMsg) bool {
	s.mu.Lock()
	now := time.Now()
	rel := m.X - 1 // dir=right：主控机在我左侧，rel∈[0,1] 从左边缘往右
	if sess.dir == "left" {
		rel = m.X + 1 // 主控机在我右侧，rel=1 为右边缘
	}
	// 进入武装：光标深入本机屏幕后，回推才有意义
	if !sess.armed {
		if (sess.dir == "right" && rel >= armThreshold) ||
			(sess.dir == "left" && rel <= 1-armThreshold) {
			sess.armed = true
		}
	}
	leave := false
	if now.After(sess.graceUntil) && sess.armed {
		beyond := (sess.dir == "right" && rel < -leaveSoft) ||
			(sess.dir == "left" && rel > 1+leaveSoft)
		hard := (sess.dir == "right" && rel < -leaveHard) ||
			(sess.dir == "left" && rel > 1+leaveHard)
		if beyond {
			sess.beyondCount++
			if hard || sess.beyondCount >= 2 {
				leave = true
			}
		} else {
			sess.beyondCount = 0
		}
	}
	s.mu.Unlock()

	if rel < 0 {
		rel = 0
	} else if rel > 1 {
		rel = 1
	}
	vx, vy, vw, vh := s.injector.ScreenBounds()
	s.injector.MoveAbs(vx+int(rel*float64(vw)), vy+int(clamp01(m.Y)*float64(vh)))
	return leave
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

	sess, err := s.masterHandshake(conn, dir)
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
	_, _, vw, vh := s.injector.ScreenBounds()
	sess.virtX = float64(cx)
	sess.virtY = float64(cy)
	entry := wireMsg{T: "enter", Dir: dir, Y: sess.virtY / float64(vh)}
	first := wireMsg{T: "move", X: sess.virtX / float64(vw), Y: sess.virtY / float64(vh)}
	select {
	case sess.sendCh <- entry:
	default:
	}
	select {
	case sess.sendCh <- first:
	default:
	}
	log.Printf("KVM：开始控制 %q（向%s），Ctrl+Alt+Shift+X 紧急退出", peer.Name, dir)

	go s.masterSender(sess)
	go s.masterReader(sess, peer.Name)
	go s.masterPinger(sess)
}

func (s *Service) masterHandshake(conn net.Conn, dir string) (*masterSession, error) {
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
		s.setCooldown(time.Second)
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
	s.mu.Lock()
	if m := s.master; m != nil {
		m.virtX += float64(dx)
		m.virtY += float64(dy)
		now := time.Now()
		if now.Sub(m.lastSent) < moveSendInterval {
			s.mu.Unlock()
			return
		}
		m.lastSent = now
		_, _, vw, vh := s.injector.ScreenBounds()
		msg := wireMsg{T: "move", X: m.virtX / float64(vw), Y: m.virtY / float64(vh)}
		select {
		case m.sendCh <- msg:
		default: // 队列满则丢弃移动事件，位置由下一事件校正
		}
		s.mu.Unlock()
		return
	}
	if s.serving != nil || time.Now().Before(s.attemptUntil) {
		s.mu.Unlock()
		return
	}

	// 本地模式：边缘推动检测（原始位移 + 真实光标位置）
	now := time.Now()
	x, _ := s.injector.CursorPos()
	vx, _, vw, _ := s.injector.ScreenBounds()
	dir, push := "", 0
	if x >= vx+vw-2 && dx > 0 {
		dir, push = "right", dx
	} else if x <= vx+1 && dx < 0 {
		dir, push = "left", -dx
	}
	if dir == "" {
		// 离开边缘足够远才清零；停在边缘的静止不重置，由超时规则清理
		if x > vx+edgeResetPixels && x < vx+vw-edgeResetPixels {
			s.pushDir, s.pushAccum = "", 0
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
		s.mu.Unlock()
		return
	}
	s.attemptUntil = now.Add(3 * time.Second)
	s.mu.Unlock()
	go s.trySwitch(dir)
}

// OnMouseButton 实现 input.Callbacks。
func (s *Service) OnMouseButton(down bool, button int, x, y int) {
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
