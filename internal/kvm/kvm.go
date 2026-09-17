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

	"copywhere/internal/diag"
	"copywhere/internal/discovery"
	"copywhere/internal/input"
)

const (
	// DefaultMoveInterval 主控端移动事件合拍间隔（≈125Hz）。原始移动在窗口内
	// 累加为一次累计位移再发送，兼顾流畅与带宽。
	DefaultMoveInterval = 8 * time.Millisecond
	// DefaultReflowStep 被控端重排（reflow）注入节拍（≈250Hz）。
	DefaultReflowStep = 4 * time.Millisecond
	// reflowHorizon 每拍排空 pending 的比例分母：每拍注入约 1/horizon，
	// 网络成批到达时约 horizon 拍内追平，空闲时以最小步继续细分平滑。
	reflowHorizon = 3

	edgePushPixels     = 8 // 本地边缘推动判定：累计推动像素（从 16 优化为 8，轻推即过）
	pushGapReset       = 600 * time.Millisecond
	edgeAdjacency      = 2 // 判断两显示器相邻的坐标容差（像素）
	edgeBand           = 6 // 光标距边缘多少像素内算"贴边"（加大到 6px 适配高分屏与斜推）
	armPixels          = 6 // 进入副机后深入多少像素才允许触发切回（从 30 优化为 6，防止死锁）
	leavePushPixels    = 8 // 切回判定：贴边累计推动像素（从 16 优化为 8，轻推即回）
	pushGapLeave       = 600 * time.Millisecond
	cooldownSwitch     = 200 * time.Millisecond // 切换成功/控制结束后的防抖短冷却（消除 1.5 秒硬性冻结）
	cooldownFailed     = 1 * time.Second        // 连接/握手失败或对端不在线的冷却
	entryMargin        = 2                      // 入口位置距共享边缘的像素
	minMonitorSpan     = 100                    // 退化显示器矩形过滤阈值（幽灵/虚拟显示器会枚举出 1x1 占位项）
	umovePushThreshold = 280.0                  // Universal 通道贴边推回切回阈值（0..65535 空间，约占 1080p 的 8px）
	pingInterval       = 1 * time.Second
	readTimeout        = 5 * time.Second
	unconfigLogEvery   = 5 * time.Second // 未配置邻居提示的节流间隔
)

// Config 是 KVM 的配置（来自 config.json）。
type Config struct {
	Port         int    // 监听端口（默认 47832）
	Left         string // 光标从本机暴露的左边缘离开时控制的节点名
	Right        string // 光标从本机暴露的右边缘离开时控制的节点名
	EntryMonitor int    // 被控入口显示器索引（-1=主显示器，默认）
	SelfID       string // 本机节点指纹（配对令牌校验随 hello 发给对端）
	// MoveInterval 主控端移动合拍间隔；<=0 使用 DefaultMoveInterval。
	MoveInterval time.Duration
	// ReflowStep 被控端重排注入节拍；<=0 使用 DefaultReflowStep，
	// 显式传 <0 的哨兵值（-1）关闭重排、恢复逐包直注。
	ReflowStep time.Duration
	// TokenForPeer 返回对端签发给本机的配对令牌（本机作为主控连接它时使用）；
	// 未配对返回 false，回退共享 token。
	TokenForPeer func(id string) (string, bool)
	// PairTokenFor 返回本机签发给指定节点的配对令牌（被控端校验对端 hello 用）；
	// 未配对返回 false，回退共享 token。
	PairTokenFor func(id string) (string, bool)
	// SpeedPercent 本机作为主控的位移速度调节百分比（100=基准 1.0x；<=0 视为 100）。
	SpeedPercent int
}

// Injector 是输入注入接口（生产环境为 input.DefaultInjector，测试用假实现）。
type Injector interface {
	MoveAbs(x, y int)
	// MoveNorm 以 Universal 归一化坐标（0..65535，铺满整个虚拟桌面）注入移动。
	// 相对通道的替代品（B4）：绝对坐标不经接收端的指针速度/加速管线。
	MoveNorm(nx, ny int)
	MoveRel(dx, dy int)
	Button(down bool, button int)
	Wheel(delta int32, horizontal bool)
	Key(vk, scan uint32, down, ext bool)
	ScreenBounds() (x, y, w, h int)
	CursorPos() (x, y int)
	Monitors() []input.Rect
	MouseSpeed() int // 系统指针速度（1..20，10=1.0x）；0=不可用
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
	ID    string  `json:"id,omitempty"` // 主控机节点指纹（被控端据此查配对令牌）
	Name  string  `json:"name,omitempty"`
	Dir   string  `json:"dir,omitempty"`
	Y     float64 `json:"y,omitempty"` // 主控机光标的垂直比例（用于入口位置）
	DX    int     `json:"dx,omitempty"`
	DY    int     `json:"dy,omitempty"`
	// AX/AY 是 Universal 通道的坐标载荷（B4，仅 T=="umove" 时有意义）：
	// 主控光标相对**切换瞬间锚点**的偏移，按主控"离场所属屏"的宽/高归一到
	// ±65535（1px ≈ 34 单位，取整误差远小于 1/50 像素）。符号与主控自己的
	// 虚拟桌面一致（右为 +X、下为 +Y），两端都不需要知道对方分辨率。
	// 用有符号数而非 0..65535 无符号：越过边缘后的"过冲"是副机贴边检测所需
	// 的信号量，必须能表达锚点之外的位置（MWB 接收端线性外推同理）。
	AX int `json:"ax,omitempty"`
	AY int `json:"ay,omitempty"`
	// NL/NR/NU/ND 是被控端回报的"可达带"（T=="band"，B4）：从入口点沿左/右/
	// 上/下在副机**入口显示器**内可达的归一化偏移（与 AX/AY 同一坐标系）。
	// 可达范围=入口屏而非整个桌面：多屏副机上按桌面计会让光标越界漫游到
	// 邻屏、钉在包围盒角上（真机事故：副屏左上角冻结）；与 MWB"一对机器
	// 一块屏"的矩阵映射一致。主控端据此钳制虚拟位置，消除贴边发散与反向
	// 倒带；旧版对端不发送此消息。
	NL int `json:"nl,omitempty"`
	NR int `json:"nr,omitempty"`
	NU int `json:"nu,omitempty"`
	ND int `json:"nd,omitempty"`
	// EX/EY/Mons 是 band 消息的几何附录（B4 修正）：入口点（吸附映射的原点）
	// 与**会话可达屏（入口显示器）**矩形。主控端据此把 Universal 虚拟位置吸附
	// 进"可达屏 ± 外推余量"——几何异常（旧版带/幽灵屏）时兜底防光标离屏：
	// 桌面包围盒里的多屏"空洞"不属于任何显示器，光标被推进去会不可见、钉在
	// 包围盒角上（真机事故：副屏左上角冻结）。
	// 旧版对端不带这些字段（JSON 缺省即零值/nil），主控自动退回包围盒钳位。
	EX   int        `json:"ex,omitempty"`
	EY   int        `json:"ey,omitempty"`
	Mons []wireRect `json:"mons,omitempty"`
	// DUX/DUY/DUW/DUH 是 band 的**桌面包围盒**附录（诊断用，B4 现场数据采集）：
	// 被控端全部显示器的并集。会话可达屏只是其中一块，光有 Mons 无法还原
	// "副屏在主屏上方/侧方"这类多屏布局——真机上判断"光标为何出现在另一块屏
	// 的角上"必须同时看到包围盒与逐屏矩形，否则日志里两端各说各话、对不上。
	// 主控端不用它做任何决策（可达范围仍是入口屏），旧版对端忽略即可。
	DUX int `json:"dux,omitempty"`
	DUY int `json:"duy,omitempty"`
	DUW int `json:"duw,omitempty"`
	DUH int `json:"duh,omitempty"`
	// RawMons 是 band 的原始逐屏矩形附录（诊断用）：**未经退化过滤**的
	// Monitors() 结果。幽灵/虚拟适配器枚举出的 1x1 矩形会被 validMonitors
	// 静默过滤掉，一旦过滤规则误伤真实显示器（入口选错屏），日志里就完全
	// 看不出来——这里保留过滤前的原样，配合 Mons 即可判断"哪块屏被丢了"。
	RawMons []wireRect `json:"rawmons,omitempty"`
	// EIdx 是本次会话选中的入口屏在 RawMons 中的下标 +1（诊断用；0=未回报，
	// JSON omitempty 无法区分"下标 0"与"缺省"，故整体偏移一位）。
	// 配合 EntryMonitor 配置即可确认"入口屏是不是选错了那块"。
	EIdx   int     `json:"eidx,omitempty"`
	B      int     `json:"b,omitempty"`
	D      int     `json:"d,omitempty"`
	Down   bool    `json:"down,omitempty"`
	H      bool    `json:"h,omitempty"`
	VK     uint32  `json:"vk,omitempty"`
	Scan   uint32  `json:"scan,omitempty"`
	Ext    bool    `json:"ext,omitempty"`
	Msg    string  `json:"msg,omitempty"`
	PW     int     `json:"pw,omitempty"`     // 被控端入口显示器物理宽，供主控折算缩放比
	PH     int     `json:"ph,omitempty"`     // 被控端入口显示器物理高（并集吸附的归一化基准）
	PScale float64 `json:"pscale,omitempty"` // 被控端入口显示器实际设置缩放比 (例如 1.0, 1.25, 1.5, 2.0)
	MW     int     `json:"mw,omitempty"`     // 主控端当前显示器物理宽
	MH     int     `json:"mh,omitempty"`     // 主控端当前显示器物理高
	MScale float64 `json:"mscale,omitempty"` // 主控端当前显示器实际设置缩放比
	// Abs 是握手能力位：主控在 hello 里声明"我能发 umove"，被控在 ok 里回
	// "我也支持"。双方都为 true 时本会话走 Universal 绝对坐标通道。
	// 旧版本对端不带此字段（JSON 缺省即 false），自动退回相对位移通道。
	Abs bool `json:"abs,omitempty"`
}

// wireRect 是线上传输的显示器矩形（band 消息的 Mons，被控端虚拟桌面像素）。
type wireRect struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

func wireRects(mons []input.Rect) []wireRect {
	if len(mons) == 0 {
		return nil
	}
	out := make([]wireRect, 0, len(mons))
	for _, m := range mons {
		out = append(out, wireRect{X: m.X, Y: m.Y, W: m.W, H: m.H})
	}
	return out
}

func wireToRects(ws []wireRect) []input.Rect {
	if len(ws) == 0 {
		return nil
	}
	out := make([]input.Rect, 0, len(ws))
	for _, w := range ws {
		out = append(out, input.Rect{X: w.X, Y: w.Y, W: w.W, H: w.H})
	}
	return out
}

type masterSession struct {
	conn     net.Conn
	w        *bufio.Writer
	sendCh   chan wireMsg
	virtX    float64 // 主控机虚拟桌面内未裁剪光标位置（像素）
	virtY    float64
	lastX    float64 // 上次发送时的位置（差值基准，相对通道用）
	lastY    float64
	lastSent time.Time
	// abs=true：握手协商成功，本会话走 Universal 通道（B4 原生 MWB 0..65535 模型）。
	abs       bool
	dir       string  // 切入方向 ("right" / "left")
	normX     float64 // 当前在受控端屏幕上的绝对归一化坐标 (0..65535)
	normY     float64 // 当前在受控端屏幕上的绝对归一化坐标 (0..65535)
	anchorW   float64 // 锚点所属显示器的宽（物理位移映射到 65535 的基准）
	anchorH   float64 // 锚点所属显示器的高
	baseGainX float64 // 跨分辨率与实际设置缩放比计算出的静态基准 X 增益（未乘 speedPct）
	baseGainY float64 // 跨分辨率与实际设置缩放比计算出的静态基准 Y 增益
	gainX     float64 // 叠加速度微调百分比后的最终 X 轴增益
	gainY     float64 // 叠加速度微调百分比后的最终 Y 轴增益
	resX      float64 // 亚像素位移结转余数（防微小位移截断丢失）
	resY      float64 // 亚像素位移结转余数
	slavePW   int     // 被控端入口显示器物理宽
	slavePH   int     // 被控端入口显示器物理高
	// lockX/lockY：MWB 光标钉住策略——主控期间把 A 机光标强制钉在触发点
	// 边缘像素，阻止精确触控板手势（PT_GESTURE2，绕过 WH_MOUSE_LL）被系统
	// 投递给 A 机边缘可滚动窗口（双滚问题）。Raw Input dx/dy 旁路独立于
	// 光标实际位置，钉住光标不影响移动数据采集与转发。
	lockX int
	lockY int
	// origY：切换触发时的原始光标 Y，主控结束时将光标复位回此行。
	origY int
	stop     chan struct{}
	stopOnce sync.Once
}

type slaveSession struct {
	conn       net.Conn
	w          *bufio.Writer
	dir        string     // "right": 主控机在我左侧；"left": 主控机在我右侧
	entry      input.Rect // 入口显示器
	entryX     int        // 入口放置的光标 X
	entryY     int        // 入口放置的光标 Y
	entryTime  time.Time
	armed      bool
	maxDist    int // 距入口位置的最大深入距离
	pushAccum  int // 贴边向外推动的累计像素
	pushLast   time.Time
	leaveEdges []input.Rect // 主控方向上暴露的边缘（切回触发区）
	endOnce    sync.Once
	abs        bool // 本会话走 Universal 通道（握手协商结果，会话内不切换）
	umoveN     int
	lastUAX    int
	lastUAY    int
	hasLastU   bool

	// 重排（reflow）注入：仅相对通道使用
	rfMu    sync.Mutex
	rfDX    int           // 待注入的累计 X 位移
	rfDY    int           // 待注入的累计 Y 位移
	rfStop  chan struct{} // 非 nil 表示 reflow 协程在运行
	rfStopO sync.Once
}

// diagRing 诊断事件环容量：3s 节流下约 45 秒的现场，够定位一次会话。
const diagRing = 16

// diagPoint 一次会话内的关键数值快照（主控/被控两端共用同一结构，字段按
// 通道语义填写；主控端的 Raw* 填"本次发送值折算回副机像素"的预期落点）。
type diagPoint struct {
	At    string  `json:"at"`
	N     int     `json:"n"`    // 会话内第几个移动包
	VirtX float64 `json:"vx"`   // 主控虚拟位置（主控像素，锚点系）；被控端填 0
	VirtY float64 `json:"vy"`   // 同上
	AX    int     `json:"ax"`   // 发出（主控）/收到（被控）的归一化偏移
	AY    int     `json:"ay"`   // 同上
	RawX  int     `json:"rawx"` // 未钳位展开后的副机像素目标
	RawY  int     `json:"rawy"` // 同上
	CurX  int     `json:"curx"` // 真实光标位置
	CurY  int     `json:"cury"` // 同上
	Move  int     `json:"move"` // 距上次快照，真实光标移动了多少（0=冻结）
	Flags string  `json:"flags,omitempty"`
}

// pushDiag 把快照压入环（保留最近 diagRing 条，旧在前）。
func pushDiag(ring []diagPoint, p diagPoint) []diagPoint {
	ring = append(ring, p)
	if len(ring) > diagRing {
		ring = ring[len(ring)-diagRing:]
	}
	return ring
}

// end 结束被控会话（幂等）：可选发送 leave 并关闭连接。
// 诊断计数与会话的回收不在此处——end 会从多个协程被调用（本连接读循环、
// 热停用 Stop、物理输入检测），统一交给 handleSlaveConn 的 defer 单点回收，
// 避免正常断线（socket 关闭→读错 return，不经 end）漏掉回收造成计数泄漏。
func (sess *slaveSession) end(writeLeave bool, reason string) {
	sess.endOnce.Do(func() {
		if writeLeave {
			sess.conn.SetWriteDeadline(time.Now().Add(time.Second))
			writeMsg(sess.w, wireMsg{T: "leave"})
		}
		sess.conn.Close()
		if sess.abs && sess.umoveN > 0 {
			log.Printf("KVM：被控会话结束（%s，共处理 %d 个 Universal 移动包）", reason, sess.umoveN)
		} else {
			log.Printf("KVM：被控会话结束（%s）", reason)
		}
	})
}

// Service 是 KVM 服务。
type Service struct {
	cfg      Config
	nodeName string
	store    *discovery.Store
	ttl      time.Duration
	injector Injector
	ctx      context.Context // Start 携带的 context；热停用后用于掐断在途切换
	ln       net.Listener    // 被控端监听（Start 赋值，Stop 同步关闭以便立即重启用）

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
	monsMu    sync.Mutex
	mons      []input.Rect
	monsValid []input.Rect // mons 过滤退化矩形后的列表（注入吸附用，静默）
}

// NewService 创建 KVM 服务。鉴权只认配对令牌（Config.PairTokenFor / TokenForPeer）。
func NewService(cfg Config, nodeName string, store *discovery.Store,
	ttl time.Duration, injector Injector) *Service {
	if cfg.Port <= 0 {
		cfg.Port = 47832
	}
	cfg.MoveInterval = normalizeMoveInterval(cfg.MoveInterval)
	cfg.ReflowStep = normalizeReflowStep(cfg.ReflowStep)
	if cfg.SpeedPercent <= 0 {
		cfg.SpeedPercent = 100
	}
	return &Service{
		cfg: cfg, nodeName: nodeName,
		store: store, ttl: ttl, injector: injector,
		ctx:      context.Background(),
		modsDown: map[uint32]bool{},
	}
}

// UpdateNeighbors 热更新左右邻居配置（无需重启，下一次切换即生效）。
func (s *Service) UpdateNeighbors(left, right string) {
	s.mu.Lock()
	s.cfg.Left = left
	s.cfg.Right = right
	s.mu.Unlock()
	log.Printf("KVM 邻居已更新：左邻 %q，右邻 %q", left, right)
}

// UpdateTunables 热更新手感参数（合拍间隔/重排节拍/速度调节百分比），无需重启：
// 主控移动在下一帧、重排在下一拍即生效（均为锁内读取）。
func (s *Service) UpdateTunables(moveInterval, reflowStep time.Duration, speedPct int) {
	s.mu.Lock()
	s.cfg.MoveInterval = normalizeMoveInterval(moveInterval)
	s.cfg.ReflowStep = normalizeReflowStep(reflowStep)
	if speedPct <= 0 {
		speedPct = 100
	}
	s.cfg.SpeedPercent = speedPct
	if m := s.master; m != nil {
		userFactor := float64(speedPct) / 100.0
		m.gainX = clampFloat(m.baseGainX*userFactor, 0.2, 5.0)
		m.gainY = clampFloat(m.baseGainY*userFactor, 0.2, 5.0)
	}
	s.mu.Unlock()
	log.Printf("KVM 手感参数已热更新：合拍 %v，重排 %v，速度微调 %d%%",
		s.cfg.MoveInterval, s.cfg.ReflowStep, speedPct)
}

// Stop 热停用 KVM：同步关闭被控端监听（保证 Stop 返回后端口可立即重新
// 绑定，支持快速关-开切换）并结束全部活动会话（主控向对端发 leave，
// 对端立即干净交还控制权）。
func (s *Service) Stop() {
	s.mu.Lock()
	ln := s.ln
	s.mu.Unlock()
	if ln != nil {
		ln.Close() // 与 Start 的 ctx.Done 关闭协程双保险，重复 Close 无害
	}
	s.mu.Lock()
	m, sv := s.master, s.serving
	s.mu.Unlock()
	// end* 内部会再取 s.mu，必须在锁外调用
	if m != nil {
		s.endMasterSession(m, true, "跨屏已停用")
	}
	if sv != nil {
		sv.end(false, "跨屏已停用")
	}
}

// currentMoveInterval / currentReflowStep 在锁内读参数。
func (s *Service) currentMoveInterval() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.MoveInterval
}

func (s *Service) currentReflowStep() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.ReflowStep
}

func normalizeMoveInterval(d time.Duration) time.Duration {
	if d <= 0 {
		return DefaultMoveInterval
	}
	return d
}

// normalizeReflowStep：0→默认；<0（哨兵）→关闭重排；>0→自定义。
func normalizeReflowStep(d time.Duration) time.Duration {
	if d == 0 {
		return DefaultReflowStep
	}
	return d
}

// inputSetSuppress 包一层避免各处直接依赖 input 包细节。
func inputSetSuppress(b bool) { input.SetSuppress(b) }

// Start 启动被控端监听，直到 ctx 结束。返回错误表示端口被占用等启动失败。
func (s *Service) Start(ctx context.Context) error {
	s.ctx = ctx // 热停用后 trySwitch 据此放弃在途切换（见 trySwitch）
	ln, err := net.Listen("tcp4", fmt.Sprintf("0.0.0.0:%d", s.cfg.Port))
	if err != nil {
		return fmt.Errorf("监听 KVM 端口 %d/tcp 失败: %w", s.cfg.Port, err)
	}
	log.Printf("KVM 被控端已监听 :%d/tcp", s.cfg.Port)
	s.refreshMonitors()
	s.mu.Lock()
	s.ln = ln // 供 Stop 同步关闭（热停用后可立即重新启用）
	s.mu.Unlock()
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

// currentValidMonitors 同 currentMonitors，但已过滤退化矩形（注入吸附用）。
// 缓存未就绪时现场过滤，绝不把幽灵矩形当作吸附目标。
func (s *Service) currentValidMonitors() []input.Rect {
	s.monsMu.Lock()
	mons, valid := s.mons, s.monsValid
	s.monsMu.Unlock()
	if mons == nil {
		return []input.Rect{{X: 0, Y: 0, W: 1920, H: 1080, Primary: true}}
	}
	if len(valid) > 0 {
		return valid
	}
	return filterDegenerate(mons)
}

func (s *Service) refreshMonitors() {
	if m := s.injector.Monitors(); len(m) > 0 {
		s.monsMu.Lock()
		s.mons = m
		s.monsValid = filterDegenerate(m) // 静默：周期刷新不能刷日志
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

// validMonitors 过滤退化显示器矩形（带一次性诊断日志）：虚拟/幽灵显示适配器
// 会被 EnumDisplayMonitors 枚举成 1x1 之类的占位项，若选作入口，MoveAbs 会把
// 光标钉在桌面角落、umove 以 1px 为展开基准会让远端光标纹丝不动（真机事故：
// "副屏左上角不能动"）。低于 minMonitorSpan 的矩形不参与入口选择/切回边缘/
// 桌面并集计算。周期性调用方（refreshMonitors）请用静默的 filterDegenerate。
func validMonitors(mons []input.Rect) []input.Rect {
	out := filterDegenerate(mons)
	if len(out) == 0 {
		log.Printf("KVM：显示器列表全部为退化矩形 %v，按原样使用", mons)
		return mons
	}
	if len(out) != len(mons) {
		log.Printf("KVM：过滤退化显示器矩形，保留 %v", out)
	}
	return out
}

// filterDegenerate 是 validMonitors 的静默核心：低于 minMonitorSpan 的矩形
// 不参与入口选择/切回边缘/桌面并集/注入吸附；全部退化时按原样返回。
func filterDegenerate(mons []input.Rect) []input.Rect {
	out := make([]input.Rect, 0, len(mons))
	for _, m := range mons {
		if m.W >= minMonitorSpan && m.H >= minMonitorSpan {
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		return mons
	}
	return out
}

// desktopUnion 返回显示器矩形的并集（虚拟桌面）。与 Monitors() 同一坐标系，
// 不与 GetSystemMetrics 混用，规避 DPI 缩放下两套坐标不一致的问题。
func desktopUnion(mons []input.Rect) (x, y, w, h int) {
	if len(mons) == 0 {
		return 0, 0, 1920, 1080
	}
	x, y = mons[0].X, mons[0].Y
	r, b := mons[0].X+mons[0].W, mons[0].Y+mons[0].H
	for _, m := range mons[1:] {
		if m.X < x {
			x = m.X
		}
		if m.Y < y {
			y = m.Y
		}
		if m.X+m.W > r {
			r = m.X + m.W
		}
		if m.Y+m.H > b {
			b = m.Y + m.H
		}
	}
	return x, y, r - x, b - y
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
	conn.SetReadDeadline(time.Now().Add(readTimeout))
	// 只设置读截止时间；写截止时间若随 SetDeadline 一起设置，会在 5s 后过期，
	// 导致后续所有 pong/leave 写入立刻 i/o timeout
	conn.SetWriteDeadline(time.Time{})
	br := bufio.NewReaderSize(conn, 16*1024)
	bw := bufio.NewWriter(conn)

	var hello wireMsg
	if err := readMsg(br, &hello); err != nil || hello.T != "hello" {
		return
	}
	// 只认配对令牌：按主控机指纹查本机签发的令牌比对
	if hello.ID == "" || s.cfg.PairTokenFor == nil {
		log.Printf("KVM 拒绝来自 %s 的连接（主控机未配对）", conn.RemoteAddr())
		writeMsg(bw, wireMsg{T: "error", Msg: "unauthorized"})
		return
	}
	expected, ok := s.cfg.PairTokenFor(hello.ID)
	if !ok || subtle.ConstantTimeCompare([]byte(hello.Token), []byte(expected)) != 1 {
		log.Printf("KVM 拒绝来自 %s 的连接（未配对或令牌无效）", conn.RemoteAddr())
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
	diag.Incr("kvm.slave.start")
	diag.Add("kvm.slave.active", 1)
	diag.SetSession("kvm_slave", map[string]any{"peer": hello.Name,
		"channel": map[bool]string{true: "universal", false: "relative"}[hello.Abs],
		"since":   time.Now().Format("15:04:05")})
	mons := validMonitors(s.injector.Monitors())
	entryMon := pickEntryMonitor(mons, s.cfg.EntryMonitor)
	// Universal 通道能力协商（B4）：主控在 hello 里声明可发 umove，本机确认
	// 自己也能解算/注入，双方都点头本会话才走绝对坐标。
	// 当被控端拥有多台显示器（len(mons) > 1）时，单屏 0..65535 绝对坐标映射会导致光标
	// 锁死在单屏范围内无法移动至副屏；此时被控端明确回执 Abs=false，使会话走
	// 支持副机多屏漫游的相对位移通道（与 MWB MoveMouseRelatively 一致）。
	// 仅在被控端为单显示器且主控主动请求 Abs 时，才走 umove 绝对通道。
	sess.abs = hello.Abs && len(mons) <= 1
	pscale := entryMon.Scale
	if pscale <= 0 {
		pscale = 1.0
	}
	writeMsg(bw, wireMsg{
		T:      "ok",
		PW:     entryMon.W,
		PH:     entryMon.H,
		PScale: pscale,
		Abs:    sess.abs,
	})
	ch := "相对位移（支持多屏漫游）"
	if sess.abs {
		ch = "Universal 0..65535"
	}
	log.Printf("KVM：开始被 %q 控制（坐标通道：%s）", hello.Name, ch)
	defer func() {
		s.stopReflow(sess) // 停重排协程并把残余位移注入，绝不泄漏
		s.mu.Lock()
		if s.serving == sess {
			s.serving = nil
			// 诊断回收单点归本 defer（end 从多协程调用且正常断线不经过它）；
			// serving==sess 的判定保证每个会话恰好回收一次，不会双重 -1。
			diag.Add("kvm.slave.active", -1)
			diag.SetSession("kvm_slave", nil)
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
			mons := validMonitors(s.injector.Monitors())
			entry := pickEntryMonitor(mons, s.cfg.EntryMonitor)
			leaveEdges := leaveEdgesFor(mons, m.Dir)
			// 桌面 = 显示器矩形并集：与 entry/ex/ey 同一坐标系，不混用
			// GetSystemMetrics，规避 DPI 缩放下两套坐标不一致。
			ux, uy, uw, uh := desktopUnion(mons)
			// 入口：入口显示器的共享边缘；垂直位置按主控比例映射到入口显示器
			// 同比例高度（m.Y 已是主控"离场所属屏"内的比例，见 entryRatio）。
			ex := entry.X + entryMargin
			if m.Dir == "left" {
				ex = entry.X + entry.W - 1 - entryMargin
			}
			ey := entry.Y + int(clamp01(m.Y)*float64(entry.H))
			if ey >= entry.Y+entry.H { // 比例=1 时防越界一行
				ey = entry.Y + entry.H - 1
			}
			// 入口点必须落在桌面并集内：入口信息异常（幽灵显示器等）时兜底钳位，
			// 绝不能把光标放到桌面外（真机事故：副屏左上角）。
			ocx, ocy := ex, ey
			ex = clampInt(ex, ux, ux+uw-1)
			ey = clampInt(ey, uy, uy+uh-1)
			if ex != ocx || ey != ocy {
				log.Printf("KVM：入口点 (%d,%d) 落在桌面并集外，已钳到 (%d,%d)（入口屏 %+v）",
					ocx, ocy, ex, ey, entry)
			}
			// 会话字段一次性在锁内写入：detectLeave 在锁内读取
			// entryX/leaveEdges/entry 等，分两批写存在数据竞争。
			s.mu.Lock()
			sess.dir = m.Dir
			sess.entry = entry
			sess.leaveEdges = leaveEdges
			sess.entryX = ex
			sess.entryTime = time.Now()
			sess.armed = false
			sess.maxDist = 0
			sess.pushAccum = 0
			sess.pushLast = time.Time{}
			sess.hasLastU = false
			s.mu.Unlock()
			// 入口重定位后旧 pending 作废（残留目标会把光标从入口点拽走）。
			sess.rfMu.Lock()
			sess.rfDX, sess.rfDY = 0, 0
			sess.rfMu.Unlock()
			s.injector.MoveAbs(ex, ey)
			log.Printf("KVM：入口显示器 (%d,%d %dx%d 主屏=%v)，光标注入到 (%d,%d)",
				sess.entry.X, sess.entry.Y, sess.entry.W, sess.entry.H,
				sess.entry.Primary, ex, ey)
		case "move":
			if sess.dir == "" || sess.abs {
				continue
			}
			s.feedMove(sess, m.DX, m.DY)
		case "umove":
			// Universal 通道（B4）。未协商成功的会话一律忽略：旧主控不会发，
			// 新主控在 abs=false 时也只发 move——出现这条即协议异常。
			if sess.dir == "" || !sess.abs {
				continue
			}
			s.feedUMove(sess, m.AX, m.AY)
		case "btn":
			s.injector.Button(m.Down, m.B)
		case "wheel":
			s.injector.Wheel(int32(m.D), m.H)
		case "key":
			s.injector.Key(m.VK, m.Scan, m.Down, m.Ext)
		case "ping":
			// 保活：必须回应，否则主控端读超时会误判连接断开
			conn.SetWriteDeadline(time.Now().Add(time.Second))
			if err := writeMsg(bw, wireMsg{T: "pong"}); err != nil {
				sess.end(false, "回应 ping 失败: "+err.Error())
				return
			}
			conn.SetWriteDeadline(time.Time{})
		default:
			// 未知消息忽略
		}
	}
}

// feedMove 处理一条相对 move 事件：合拍关闭时保持旧的逐包直注语义，
// 开启时并入待注入位移并保证 reflow 协程在跑。
// 切回检测在注入之后对"实际位移"执行，与直注模式时序等价。
func (s *Service) feedMove(sess *slaveSession, dx, dy int) {
	diag.Incr("kvm.move.recv")
	if s.cfg.ReflowStep < 0 {
		s.injector.MoveRel(dx, dy)
		if s.detectLeave(sess, dx) {
			sess.end(true, "光标推回共享边缘")
		}
		return
	}
	s.startReflow(sess)
	sess.rfMu.Lock()
	sess.rfDX += dx
	sess.rfDY += dy
	sess.rfMu.Unlock()
}

// feedUMove 处理一条 Universal umove 事件（B4 0..65535 模型）。
// 单屏环境下直接按入口显示器映射为本地物理像素并通过 MoveAbs (SetCursorPos) 注入；
// 多屏环境下将归一化增量转换为像素位移转交 feedMove 处理，支持副屏自由漫游。
func (s *Service) feedUMove(sess *slaveSession, ax, ay int) {
	diag.Incr("kvm.umove.recv")
	mons := validMonitors(s.injector.Monitors())
	if len(mons) > 1 {
		if !sess.hasLastU {
			sess.hasLastU = true
			sess.lastUAX = ax
			sess.lastUAY = ay
			return
		}
		dax := ax - sess.lastUAX
		day := ay - sess.lastUAY
		sess.lastUAX = ax
		sess.lastUAY = ay

		w := sess.entry.W
		h := sess.entry.H
		if w < 2 {
			w = 2
		}
		if h < 2 {
			h = 2
		}
		pdx := roundMulDiv(dax, w-1, 65535)
		pdy := roundMulDiv(day, h-1, 65535)
		if pdx != 0 || pdy != 0 {
			s.feedMove(sess, pdx, pdy)
		}
		return
	}

	w := sess.entry.W
	h := sess.entry.H
	if w < 2 {
		w = 2
	}
	if h < 2 {
		h = 2
	}
	tx := sess.entry.X + roundMulDiv(ax, w-1, 65535)
	ty := sess.entry.Y + roundMulDiv(ay, h-1, 65535)
	tx = clampInt(tx, sess.entry.X, sess.entry.X+w-1)
	ty = clampInt(ty, sess.entry.Y, sess.entry.Y+h-1)

	s.injector.MoveAbs(tx, ty)
	sess.umoveN++
	if sess.umoveN == 1 || sess.umoveN%150 == 0 {
		cx, cy := s.injector.CursorPos()
		log.Printf("KVM：Universal 移动 #%d (AX=%d, AY=%d)，目标 (%d,%d)，真实光标 (%d,%d)",
			sess.umoveN, ax, ay, tx, ty, cx, cy)
	}
}

// roundMulDiv 计算 a*b/c 并四舍五入（a≥0，c>0）。
func roundMulDiv(a, b, c int) int {
	return (a*b + c/2) / c
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// startReflow 惰性启动本会话的 reflow 协程（move 首次到达时）。
func (s *Service) startReflow(sess *slaveSession) {
	sess.rfMu.Lock()
	if sess.rfStop != nil {
		sess.rfMu.Unlock()
		return
	}
	sess.rfStop = make(chan struct{})
	sess.rfStopO = sync.Once{}
	stop := sess.rfStop
	sess.rfMu.Unlock()
	go s.reflowLoop(sess, stop)
}

// stopReflow 结束 reflow 协程并把残余位移立即注入，保证总位移守恒。
func (s *Service) stopReflow(sess *slaveSession) {
	sess.rfMu.Lock()
	stop := sess.rfStop
	sess.rfStop = nil
	sess.rfMu.Unlock()
	if stop == nil {
		return
	}
	sess.rfStopO.Do(func() { close(stop) })
}

// flushReflow 立即把 pending 注入排空（相对通道），供会话结束等路径使用。
// 返回相对通道实际注入的 dx,dy。
func (s *Service) flushReflow(sess *slaveSession) (int, int) {
	sess.rfMu.Lock()
	dx, dy := sess.rfDX, sess.rfDY
	sess.rfDX, sess.rfDY = 0, 0
	sess.rfMu.Unlock()
	if dx != 0 || dy != 0 {
		s.injector.MoveRel(dx, dy)
	}
	return dx, dy
}

// reflowLoop 按固定节拍重排注入（仅相对通道使用）。相对通道每拍排空 pending 位移的约
// 1/reflowHorizon（有符号、保底各 1px 防滞留），位移总和精确守恒。
func (s *Service) reflowLoop(sess *slaveSession, stop chan struct{}) {
	t := time.NewTicker(s.currentReflowStep())
	defer t.Stop()
	for {
		select {
		case <-stop:
			s.flushReflow(sess)
			return
		case <-t.C:
			sess.rfMu.Lock()
			dx, dy := sess.rfDX, sess.rfDY
			if dx == 0 && dy == 0 {
				sess.rfStop = nil
				sess.rfMu.Unlock()
				return // 队列排空：退出，等下一个 move 惰性重启
			}
			sx := reflowStep(dx)
			sy := reflowStep(dy)
			sess.rfDX -= sx
			sess.rfDY -= sy
			sess.rfMu.Unlock()
			s.injector.MoveRel(sx, sy)
			if s.detectLeave(sess, sx) {
				sess.end(true, "光标推回共享边缘")
				return
			}
		}
	}
}

// reflowStep 计算单拍步长：约 1/reflowHorizon，向零取整、非零时保底 1px。
func reflowStep(v int) int {
	step := v / reflowHorizon
	if v > 0 && step < 1 {
		step = 1
	}
	if v < 0 && step > -1 {
		step = -1
	}
	return step
}

// detectLeave 检测"光标推回共享边缘"的切回动作。光标为真实位置，
// 副机本机多显示器由操作系统自然跨越，因此只需监测暴露边缘。
func (s *Service) detectLeave(sess *slaveSession, dx int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()

	cx, cy := s.injector.CursorPos()
	// 深入距离武装：光标离开共享边缘向屏幕内移动足够远后才允许触发切回
	inward := 0
	if sess.dir == "right" {
		inward = cx - sess.entryX
	} else {
		inward = sess.entryX - cx
	}
	if inward > sess.maxDist {
		sess.maxDist = inward
	}
	if !sess.armed {
		if sess.maxDist >= armPixels {
			sess.armed = true
		} else {
			return false
		}
	}

	atEdge := false
	for _, r := range sess.leaveEdges {
		inY := cy >= r.Y && cy < r.Y+r.H
		if sess.dir == "right" && inY && cx <= r.X+edgeBand {
			atEdge = true
			break
		}
		if sess.dir == "left" && inY && cx >= r.X+r.W-1-edgeBand {
			atEdge = true
			break
		}
	}
	if !atEdge {
		if now.Sub(sess.pushLast) > pushGapLeave {
			sess.pushAccum = 0
		}
		sess.pushLast = now
		return false
	}
	// 向外推为正、向内推为负：双向都作用于同一累积值，贴边微抖只会
	// 相互抵消，不会出现"向内一抖就整笔清零"或"只增不减导致误触"。
	delta := 0
	if sess.dir == "right" {
		delta = -dx // dir=right：主控在左侧，向外=向左推
	} else {
		delta = dx
	}
	sess.pushAccum += delta
	if sess.pushAccum < 0 {
		sess.pushAccum = 0
	}
	sess.pushLast = now
	return sess.pushAccum >= leavePushPixels
}

// ---------- 主控端（master） ----------

// trySwitch 尝试把控制权切到 dir 方向的邻居（"left"/"right"，按节点名匹配）。
func (s *Service) trySwitch(dir string) {
	s.mu.Lock()
	// 已在控制/被控或已有切换在进行中：直接放弃（触发窗口过后
	// 重复推动可能并发进入这里，避免建立第二个主控会话）。
	if s.master != nil || s.serving != nil {
		s.mu.Unlock()
		return
	}
	left, right := s.cfg.Left, s.cfg.Right
	s.mu.Unlock()
	var peer *discovery.Peer
	for _, p := range s.store.Alive(s.ttl) {
		if (dir == "right" && p.Name == right) ||
			(dir == "left" && p.Name == left) {
			pp := p
			peer = &pp
			break
		}
	}
	if peer == nil {
		log.Printf("KVM：%s 方向的邻居 %q 不在线", dir,
			map[bool]string{true: right, false: left}[dir == "right"])
		s.setCooldown(cooldownFailed)
		return
	}
	// 只与已配对的节点跨屏
	if s.cfg.TokenForPeer == nil {
		log.Printf("KVM：跳过 %q（未配对）", peer.Name)
		s.setCooldown(cooldownFailed)
		return
	}
	if _, ok := s.cfg.TokenForPeer(peer.ID); !ok {
		log.Printf("KVM：跳过 %q（未配对，请先在面板中配对）", peer.Name)
		s.setCooldown(cooldownFailed)
		return
	}

	// 热停用后绝不能再建立会话：trySwitch 在独立 goroutine 中运行，
	// 拨号+握手最长可达 3 秒，期间用户可能已关闭跨屏。若不检查，
	// 僵尸服务会在停用后抢占 master 并置位输入抑制，而 App 层门禁
	// 已不再把事件转给本服务，没有任何路径能复位——本机鼠标键盘被吞。
	select {
	case <-s.ctx.Done():
		log.Printf("KVM：跨屏已停用，放弃切换到 %q（%s）", peer.Name, dir)
		return
	default:
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
		s.setCooldown(cooldownFailed)
		return
	}

	cx, cy := s.injector.CursorPos()
	curMon := s.currentMonitorAt(cx, cy)
	aw, ah := float64(curMon.W), float64(curMon.H)
	if aw < 1 {
		aw = 1920
	}
	if ah < 1 {
		ah = 1080
	}
	ny := s.entryRatio(float64(cx), float64(cy))

	sess, err := s.masterHandshake(conn, peer.ID, curMon)
	if err != nil {
		conn.Close()
		log.Printf("KVM：与 %q 握手失败: %v", peer.Name, err)
		s.setCooldown(cooldownFailed)
		return
	}

	// 注册前再查一次（拨号+握手期间可能已停用）：此刻尚未注册 master、
	// 未置位输入抑制，直接关连接即可干净放弃。
	select {
	case <-s.ctx.Done():
		conn.Close()
		log.Printf("KVM：跨屏已停用，放弃切换到 %q（%s）", peer.Name, dir)
		return
	default:
	}

	s.mu.Lock()
	sess.dir = dir
	sess.virtX, sess.virtY = float64(cx), float64(cy)
	sess.lastX, sess.lastY = sess.virtX, sess.virtY
	sess.anchorW, sess.anchorH = aw, ah
	if dir == "right" {
		sess.normX = 0
	} else {
		sess.normX = 65535
	}
	sess.normY = clamp01(ny) * 65535.0
	// MWB 光标钉住：记录锁定目标。
	// lockX = cx（边缘 X，保持切换触发位置）
	// lockY = 屏幕底部（通常是任务栏区域，非 pointer-aware 窗口）。
	// 系统手势（WM_POINTERWHEEL）将投递给任务栏而不是可滚动内容区。
	// origY 保存原始光标行，用于主控结束时复位。
	sess.lockX = cx
	sess.lockY = curMon.Y + curMon.H - 1
	sess.origY = cy
	s.master = sess
	s.mu.Unlock()
	diag.Incr("kvm.master.start")
	diag.SetSession("kvm_master", map[string]any{"peer": peer.Name, "dir": dir,
		"channel": map[bool]string{true: "universal", false: "relative"}[sess.abs],
		"since":   time.Now().Format("15:04:05")})
	inputSetSuppress(true)
	// MWB 光标钉住：先移动光标到底部，再用 ClipCursor 锁死。
	// 顺序很重要：先移位再锁，否则 ClipCursor 生效后、MoveAbs 前
	// 存在短暂窗口期光标仍在原始边缘位置（可滚动区）。
	s.injector.MoveAbs(sess.lockX, sess.lockY)
	input.ClipCursorTo(sess.lockX, sess.lockY)
	if sess.abs {
		log.Printf("KVM：本会话走 Universal 0..65535 绝对注入（锚点 %d×%d 屏，缩放补偿 X%.2f/Y%.2f）",
			int(aw), int(ah), sess.gainX, sess.gainY)
	} else {
		log.Printf("KVM：控制通道：相对位移（跨分辨率与缩放比静态补偿 X%.2f/Y%.2f，支持副机多屏漫游）",
			sess.gainX, sess.gainY)
	}
	select {
	case sess.sendCh <- wireMsg{T: "enter", Dir: dir, Y: ny}:
	default:
	}
	log.Printf("KVM：开始控制 %q（向%s），入口比例 %.2f，Ctrl+Alt+Shift+X 紧急退出",
		peer.Name, dir, ny)

	go s.masterSender(sess)
	go s.masterReader(sess, peer.Name)
	go s.masterPinger(sess)
}

// currentMonitorAt 返回光标 (x,y) 所属显示器；找不到时退回主显示器，
// 再退回默认 1920x1080 100% 缩放显示器。
func (s *Service) currentMonitorAt(x, y int) input.Rect {
	var primary input.Rect
	for _, m := range s.currentMonitors() {
		if m.Primary {
			primary = m
		}
		if x >= m.X && x < m.X+m.W && y >= m.Y && y < m.Y+m.H {
			return m
		}
	}
	if primary.W > 0 && primary.H > 0 {
		return primary
	}
	return input.Rect{X: 0, Y: 0, W: 1920, H: 1080, Primary: true, Scale: 1.0, DPI: 96}
}

// monitorWHAt 返回光标 (x,y) 所属显示器的物理宽高（与 entryRatio 用同一块屏
// 为基准）——Universal 通道以它作 ±65535 归一化分母。
func (s *Service) monitorWHAt(x, y int) (float64, float64) {
	m := s.currentMonitorAt(x, y)
	return math.Max(float64(m.W), 1), math.Max(float64(m.H), 1)
}

// entryRatio 计算光标 (x,y) 在其所属显示器内的高度比例（0..1）。
// 主控多屏时，该比例才反映光标在当前工作屏上的位置；无匹配显示器时
// 退回以整个虚拟桌面高度估算。
func (s *Service) entryRatio(x, y float64) float64 {
	for _, m := range s.currentMonitors() {
		if x >= float64(m.X) && x < float64(m.X+m.W) &&
			y >= float64(m.Y) && y < float64(m.Y+m.H) {
			if m.H > 0 {
				return clamp01((y - float64(m.Y)) / float64(m.H))
			}
		}
	}
	_, _, _, vh := s.injector.ScreenBounds()
	if vh > 0 {
		return clamp01(y / float64(vh))
	}
	return 0.5
}

func (s *Service) masterHandshake(conn net.Conn, peerID string, curMon input.Rect) (*masterSession, error) {
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReaderSize(conn, 16*1024)
	bw := bufio.NewWriter(conn)
	// 只认配对令牌：对端签发给本机的令牌
	if peerID == "" || s.cfg.TokenForPeer == nil {
		return nil, fmt.Errorf("未配对")
	}
	token, ok := s.cfg.TokenForPeer(peerID)
	if !ok {
		return nil, fmt.Errorf("未配对")
	}
	mscale := curMon.Scale
	if mscale <= 0 {
		mscale = 1.0
	}
	mw := curMon.W
	mh := curMon.H
	if mw <= 0 {
		mw = 1920
	}
	if mh <= 0 {
		mh = 1080
	}
	if err := writeMsg(bw, wireMsg{
		T:      "hello",
		Token:  token,
		ID:     s.cfg.SelfID,
		Name:   s.nodeName,
		MW:     mw,
		MH:     mh,
		MScale: mscale,
		Abs:    false,
	}); err != nil {
		return nil, err
	}
	var resp wireMsg
	if err := readMsg(br, &resp); err != nil {
		return nil, err
	}
	if resp.T != "ok" {
		return nil, fmt.Errorf("%s", resp.Msg)
	}

	s.mu.Lock()
	speedPct := s.cfg.SpeedPercent
	s.mu.Unlock()

	pscale := resp.PScale
	if pscale <= 0 {
		pscale = 1.0
	}
	baseGx, baseGy := calcStaticGain(mw, mh, mscale, resp.PW, resp.PH, pscale, 100)
	gx, gy := calcStaticGain(mw, mh, mscale, resp.PW, resp.PH, pscale, speedPct)

	sess := &masterSession{
		conn:      conn,
		w:         bw,
		sendCh:    make(chan wireMsg, 512),
		stop:      make(chan struct{}),
		slavePW:   resp.PW,  // 被控端入口显示器物理宽（旧版对端为 0）
		slavePH:   resp.PH,  // 被控端入口显示器物理高（并集吸附基准；旧版为 0）
		abs:       resp.Abs, // Universal 通道：两端都声明支持才启用（B4）
		baseGainX: baseGx,
		baseGainY: baseGy,
		gainX:     gx,
		gainY:     gy,
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
			if m.T == "move" || m.T == "umove" {
				diag.Incr("kvm.move.sent")
			}
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
		case "band":
			// 兼容旧对端的回报消息，忽略即可
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
		input.ClipCursorRelease()
		// MWB 光标复位：解除锁定后把光标复位回边缘原始行。
		// lockX 是边缘 X，origY 是切换前的 Y，避免光标因锁定而残留在屏幕底部。
		s.injector.MoveAbs(sess.lockX, sess.origY)
		if sendLeave && sess.conn != nil && sess.w != nil {
			sess.conn.SetWriteDeadline(time.Now().Add(time.Second))
			writeMsg(sess.w, wireMsg{T: "leave"})
		}
		if sess.conn != nil {
			sess.conn.Close()
		}
		s.mu.Lock()
		if s.master == sess {
			s.master = nil
			// 与从端同构：diag 回收只由唯一的所有者执行。若 trySwitch 已把
			// s.master 换成新会话，这里不得再用 nil 抹掉新会话的登记。
			diag.SetSession("kvm_master", nil)
		}
		s.mu.Unlock()
		s.setCooldown(cooldownSwitch)
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
		if m.abs {
			// MWB 原生绝对坐标模型：
			// normX, normY 直接追踪光标在受控端屏幕上的绝对位置（0..65535）。
			// 物理位移 dx, dy 映射到 65535 空间：
			m.normX += float64(dx) * 65535.0 / m.anchorW
			m.normY += float64(dy) * 65535.0 / m.anchorH
			if m.normY < 0 {
				m.normY = 0
			} else if m.normY > 65535 {
				m.normY = 65535
			}

			// 边缘推回检测（切回主控机）：
			// 阈值设为 umovePushThreshold（500，约占屏幕 0.76%，在 1080p 下约 8~15px），防止微小手抖误切回
			if m.dir == "right" {
				// 入口在副机左侧 (0)：向左推越过入口阈值即切回
				if m.normX < -umovePushThreshold {
					s.mu.Unlock()
					s.endMasterSession(m, true, "光标推回共享边缘")
					return
				}
				if m.normX > 65535 {
					m.normX = 65535 // 副机右侧未配置跨屏，钳在右边缘
				}
			} else if m.dir == "left" {
				// 入口在副机右侧 (65535)：向右推越过入口阈值即切回
				if m.normX > 65535.0+umovePushThreshold {
					s.mu.Unlock()
					s.endMasterSession(m, true, "光标推回共享边缘")
					return
				}
				if m.normX < 0 {
					m.normX = 0 // 副机左侧未配置跨屏，钳在左边缘
				}
			}

			now := time.Now()
			if now.Sub(m.lastSent) < s.cfg.MoveInterval {
				s.mu.Unlock()
				return
			}
			m.lastSent = now
			ax := clampInt(int(math.Round(m.normX)), 0, 65535)
			ay := clampInt(int(math.Round(m.normY)), 0, 65535)
			msg := wireMsg{T: "umove", AX: ax, AY: ay}
			select {
			case m.sendCh <- msg:
			default: // 队列满则丢弃移动事件，位移由后续事件补足
			}
			s.mu.Unlock()
			return
		}

		// 相对通道：静态跨分辨率与实际设置缩放比补偿 + 亚像素余数结转，
		// 绝不随手速漂移，副机操作系统自然处理多显示器跨屏。
		m.virtX += float64(dx) * m.gainX
		m.virtY += float64(dy) * m.gainY
		now := time.Now()
		if now.Sub(m.lastSent) < s.cfg.MoveInterval {
			s.mu.Unlock()
			return
		}
		m.lastSent = now
		rdx := m.virtX - m.lastX
		rdy := m.virtY - m.lastY
		m.lastX, m.lastY = m.virtX, m.virtY

		fx := rdx + m.resX
		fy := rdy + m.resY
		ddx := int(math.Round(fx))
		ddy := int(math.Round(fy))
		m.resX = fx - float64(ddx)
		m.resY = fy - float64(ddy)

		if ddx != 0 || ddy != 0 {
			msg := wireMsg{T: "move", DX: ddx, DY: ddy}
			select {
			case m.sendCh <- msg:
			default: // 队列满则丢弃移动事件，位移由后续事件补足
			}
		}
		s.mu.Unlock()
		return
	}
	if s.serving != nil || time.Now().Before(s.attemptUntil) || time.Now().Before(s.cooldownUntil) {
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
		// 反向移动抵消累积（下限 0）而非整笔清零：高回报率鼠标贴边斜推时
		// dx 常在 ±1 间抖动，整笔清零会让累积永远到不了阈值——表现为
		// "贴边有时能触发、有时怎么推都没反应"。
		if s.pushAccum > 0 {
			if (s.pushDir == "right" && dx < 0) || (s.pushDir == "left" && dx > 0) {
				s.pushAccum -= abs(dx)
				if s.pushAccum < 0 {
					s.pushAccum = 0
				}
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
	// 触发窗口与成功防抖短冷却一致（200ms）：切换成功或结束控制后瞬间即可再次滑入，
	// 彻底消除 1.5 秒死卡；切换进行中的重复触发由 trySwitch 入口守卫兜底。
	s.attemptUntil = now.Add(cooldownSwitch)
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

func clampFloat(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// calcStaticGain 根据主控当前显示器 (mw, mh, mscale)、被控入口显示器 (pw, ph, pscale)
// 以及用户自定义速度系数百分比 (speedPct, 100=1.0x) 计算静态跨分辨率与缩放比位移增益。
//
// 核心数学推导：
// 人眼在主控端实际操作的有效逻辑视口为 Lm_x = mw / mscale, Lm_y = mh / mscale。
// 被控端目标物理跨度为 pw, ph。
// 为消除高分屏高缩放导致的失真，静态基准增益为：
//
//	gainX = pw / Lm_x = (pw * mscale) / mw
//	gainY = ph / Lm_y = (ph * mscale) / mh
//
// 再叠加用户自定义百分比 speedPct / 100.0，并在 [0.2, 5.0] 安全区间钳位。
func calcStaticGain(mw, mh int, mscale float64, pw, ph int, pscale float64, speedPct int) (float64, float64) {
	if mw <= 0 {
		mw = 1920
	}
	if mh <= 0 {
		mh = 1080
	}
	if mscale <= 0 {
		mscale = 1.0
	}
	if pw <= 0 {
		pw = mw
	}
	if ph <= 0 {
		ph = mh
	}
	if pscale <= 0 {
		pscale = 1.0
	}
	if speedPct <= 0 {
		speedPct = 100
	}

	lmX := float64(mw) / mscale
	lmY := float64(mh) / mscale

	baseX := float64(pw) / lmX
	baseY := float64(ph) / lmY

	userFactor := float64(speedPct) / 100.0
	gx := clampFloat(baseX*userFactor, 0.2, 5.0)
	gy := clampFloat(baseY*userFactor, 0.2, 5.0)
	return gx, gy
}
