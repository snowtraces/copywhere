package kvm

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"copywhere/internal/discovery"
	"copywhere/internal/input"
)

// fakeInjector 记录注入调用，模拟双屏：副屏(-1920,0) + 主屏(0,0)，均 1920x1080。
type fakeInjector struct {
	mu      sync.Mutex
	abs     [][2]int
	norms   [][2]int // MoveNorm 收到的 0..65535 归一化坐标
	rels    [][2]int
	clicks  []string
	wheels  []int32
	keys    []uint32
	cursorX int
	cursorY int
	speed   int // MouseSpeed 返回值（0=不补偿，保持既有测试语义）
	// monsOverride 非 nil 时替代默认显示器列表（模拟幽灵显示器等异常布局）
	monsOverride []input.Rect
}

func newFakeInjector() *fakeInjector { return &fakeInjector{} }

func (f *fakeInjector) MoveAbs(x, y int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.abs = append(f.abs, [2]int{x, y})
	f.cursorX, f.cursorY = x, y // 绝对注入会移动光标
}
func (f *fakeInjector) MoveRel(dx, dy int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rels = append(f.rels, [2]int{dx, dy})
	f.cursorX += dx // 真实光标随相对注入移动
	f.cursorY += dy
}

// MoveNorm 模拟 MOUSEEVENTF_ABSOLUTE|VIRTUALDESKTOP 注入：按与
// kvm.injectPixelsNorm 互逆的端点对齐映射展开为像素并移动光标，
// 使 detectLeave 等真实光标路径在 Universal 模式下同样可测。
func (f *fakeInjector) MoveNorm(nx, ny int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.norms = append(f.norms, [2]int{nx, ny})
	vx, vy, vw, vh := f.ScreenBounds()
	f.cursorX = vx + (nx*(vw-1)+32767)/65535
	f.cursorY = vy + (ny*(vh-1)+32767)/65535
}
func (f *fakeInjector) Button(down bool, button int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := "up"
	if down {
		s = "down"
	}
	f.clicks = append(f.clicks, fmt.Sprintf("%d-%s", button, s))
}
func (f *fakeInjector) Wheel(delta int32, horizontal bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.wheels = append(f.wheels, delta)
}
func (f *fakeInjector) Key(vk, scan uint32, down, ext bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = append(f.keys, vk)
}
func (f *fakeInjector) ScreenBounds() (int, int, int, int) { return -1920, 0, 3840, 1080 }
func (f *fakeInjector) CursorPos() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cursorX, f.cursorY
}
func (f *fakeInjector) Monitors() []input.Rect {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.monsOverride != nil {
		return f.monsOverride
	}
	return []input.Rect{
		{X: -1920, Y: 0, W: 1920, H: 1080},            // 副屏（非主屏）
		{X: 0, Y: 0, W: 1920, H: 1080, Primary: true}, // 主屏
	}
}

func (f *fakeInjector) relCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rels)
}

// relSum 返回所有相对注入的累计位移 (Σdx, Σdy)，用于验证重排位移守恒。
func (f *fakeInjector) relSum() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var sx, sy int
	for _, r := range f.rels {
		sx += r[0]
		sy += r[1]
	}
	return sx, sy
}

// maxRelStep 返回单拍最大绝对位移，用于验证重排把大步拆成了小步。
func (f *fakeInjector) maxRelStep() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := 0
	for _, r := range f.rels {
		for _, v := range [2]int{r[0], r[1]} {
			if v < 0 {
				v = -v
			}
			if v > m {
				m = v
			}
		}
	}
	return m
}
func (f *fakeInjector) MouseSpeed() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.speed
}
func (f *fakeInjector) absCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.abs)
}
func (f *fakeInjector) normCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.norms)
}
func (f *fakeInjector) lastNorm() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.norms[len(f.norms)-1][0], f.norms[len(f.norms)-1][1]
}
func (f *fakeInjector) lastPixel() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cursorX, f.cursorY
}
func (f *fakeInjector) lastAbs() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.abs[len(f.abs)-1][0], f.abs[len(f.abs)-1][1]
}
func (f *fakeInjector) setCursor(x, y int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cursorX, f.cursorY = x, y
}

func newTestService(t *testing.T, inj Injector) (*Service, int) {
	t.Helper()
	// 默认关闭重排（ReflowStep<0），保持逐包直注语义，便于既有测试确定性地
	// 断言"整包位移即注入位移"。重排行为由 newTestServiceReflow 单独覆盖。
	return newTestServiceCfg(t, inj, Config{ReflowStep: -1})
}

// newTestServiceReflow 启用重排注入（reflow），用于验证拆步/守恒/追平。
func newTestServiceReflow(t *testing.T, inj Injector, step time.Duration) (*Service, int) {
	t.Helper()
	return newTestServiceCfg(t, inj, Config{ReflowStep: step})
}

func newTestServiceCfg(t *testing.T, inj Injector, over Config) (*Service, int) {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	store := discovery.NewStore("self")
	svc := NewService(Config{
		Port: port, EntryMonitor: -1, SelfID: "SLAVE-ID",
		MoveInterval: over.MoveInterval,
		ReflowStep:   over.ReflowStep,
		// 测试约定：主控机指纹 MASTER-ID 的配对令牌为 "tok"
		PairTokenFor: func(id string) (string, bool) { return "tok", id == "MASTER-ID" },
	}, "SLAVE", store, 12*time.Second, inj)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return svc, port
}

type testMaster struct {
	conn net.Conn
	r    *bufio.Reader
}

func dialMaster(t *testing.T, port int, token string) *testMaster {
	t.Helper()
	return dialMasterCap(t, port, token, false)
}

// dialMasterCap 发起握手，abs 声明本机（测试里扮演的主控端）能否走 Universal
// 通道；返回后可从 recv() 读到对端的 ok/error 应答（其 Abs 反映协商结果）。
func dialMasterCap(t *testing.T, port int, token string, abs bool) *testMaster {
	t.Helper()
	conn, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", port), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	bw := bufio.NewWriter(conn)
	b, _ := json.Marshal(wireMsg{T: "hello", Token: token, ID: "MASTER-ID", Name: "MASTER", Abs: abs})
	bw.Write(append(b, '\n'))
	bw.Flush()
	return &testMaster{conn: conn, r: bufio.NewReader(conn)}
}

func (tm *testMaster) send(m wireMsg) error {
	b, _ := json.Marshal(m)
	_, err := tm.conn.Write(append(b, '\n'))
	return err
}

func (tm *testMaster) recv() (wireMsg, error) {
	line, err := tm.r.ReadSlice('\n')
	if err != nil {
		return wireMsg{}, err
	}
	var m wireMsg
	if err := json.Unmarshal(trimEOL(line), &m); err != nil {
		return wireMsg{}, err
	}
	return m, nil
}

func TestExposedEdgesDualMonitor(t *testing.T) {
	inj := newFakeInjector()
	mons := inj.Monitors()
	left, right := exposedEdges(mons)
	// 副屏在左：副屏左边缘暴露；主屏右边缘暴露；内侧两两相邻不暴露
	if len(left) != 1 || left[0].X != -1920 {
		t.Fatalf("左暴露应为副屏, got %+v", left)
	}
	if len(right) != 1 || right[0].X != 0 {
		t.Fatalf("右暴露应为主屏, got %+v", right)
	}
}

// 回归：入口显示器默认恒为主屏，即使副屏占据了暴露的共享边缘侧；
// 显式配置 kvm_entry_monitor 时使用指定下标。
func TestPickEntryMonitor(t *testing.T) {
	inj := newFakeInjector()
	mons := inj.Monitors() // 副屏(-1920)在主屏左侧，暴露左边缘只有副屏

	entry := pickEntryMonitor(mons, -1)
	if !entry.Primary || entry.X != 0 {
		t.Fatalf("默认入口应恒为主屏, got %+v", entry)
	}

	entry = pickEntryMonitor(mons, 0) // 显式指定第 0 块（副屏）
	if entry.X != -1920 {
		t.Fatalf("显式指定下标应返回对应显示器, got %+v", entry)
	}
}

// 退化显示器矩形（幽灵/虚拟显示器适配器枚举出的 1x1 占位项）必须被过滤：
// 真机事故——带 Primary 标志的 1x1 幽灵屏被选作入口，光标被 MoveAbs 钉到
// 桌面左上角，且 umove 以 1px 为展开基准，远端光标完全无法移动。
func TestValidMonitorsFiltersDegenerate(t *testing.T) {
	mons := []input.Rect{
		{X: 0, Y: 0, W: 1, H: 1, Primary: true}, // 幽灵占位屏（却是 Primary）
		{X: 0, Y: 0, W: 1920, H: 1080},          // 真实工作屏（非 Primary）
	}
	got := validMonitors(mons)
	if len(got) != 1 || got[0].W != 1920 {
		t.Fatalf("1x1 幽灵屏应被过滤: %+v", got)
	}
	entry := pickEntryMonitor(got, -1)
	if entry.W != 1920 {
		t.Fatalf("应回退到唯一有效屏: %+v", entry)
	}
	// 全部退化时按原样返回（并打日志），绝不能静默返回空列表
	all := []input.Rect{{X: 0, Y: 0, W: 1, H: 1}}
	if got := validMonitors(all); len(got) != 1 {
		t.Fatalf("全退化时应原样返回: %+v", got)
	}
}

func TestSlaveEntryOnPrimarySharedEdge(t *testing.T) {
	inj := newFakeInjector()
	_, port := newTestService(t, inj)

	tm := dialMaster(t, port, "tok")
	if _, err := tm.recv(); err != nil { // ok
		t.Fatal(err)
	}
	// 主控机从其右边缘切出（dir=right）→ 我方入口在其主显示器左边缘；
	// 垂直位置跟随主控机光标高度比例（0.5 → 居中）
	if err := tm.send(wireMsg{T: "enter", Dir: "right", Y: 0.5}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return inj.absCount() > 0 }, "入口绝对注入")
	x, y := inj.lastAbs()
	if x != 0+entryMargin || y != 540 {
		t.Fatalf("入口坐标错误: got (%d,%d), want (2,540)", x, y)
	}
}

// 回归：入口高度必须是被控主屏"同比例"位置，而非固定居中或角落。
func TestSlaveEntryFollowsProportionalHeight(t *testing.T) {
	cases := []struct {
		y    float64
		want int
	}{
		{0.0, 0},
		{0.25, 270},
		{0.75, 810},
		{1.0, 1079}, // 比例=1 落在主屏最后一行，不越界
	}
	for _, c := range cases {
		inj := newFakeInjector()
		_, port := newTestService(t, inj)
		tm := dialMaster(t, port, "tok")
		if _, err := tm.recv(); err != nil {
			t.Fatal(err)
		}
		if err := tm.send(wireMsg{T: "enter", Dir: "right", Y: c.y}); err != nil {
			t.Fatal(err)
		}
		waitFor(t, func() bool { return inj.absCount() > 0 }, "入口绝对注入")
		x, y := inj.lastAbs()
		if x != 0+entryMargin || y != c.want {
			t.Fatalf("比例 %.2f：入口坐标 got (%d,%d), want (%d,%d)",
				c.y, x, y, entryMargin, c.want)
		}
	}
}

// 回归：主控多屏时，入口比例应以"光标离场所属屏"为基准，而非整个虚拟桌面。
// 副屏(-1920,0) + 主屏(0,0) 均 1920x1080，虚拟桌面高仍为 1080，
// 若误用整桌面宽度/原点在 -1920 的坐标算高度比例会得同样结果，故用主屏
// 下半（y=810）验证比例 = 0.75，且副屏上半（y=270, x=-960）应给 0.25。
func TestEntryRatioUsesSourceMonitor(t *testing.T) {
	inj := newFakeInjector()
	svc, _ := newTestService(t, inj)
	if got := svc.entryRatio(960, 810); got != 0.75 {
		t.Fatalf("主屏 y=810 比例应为 0.75, got %.3f", got)
	}
	if got := svc.entryRatio(-960, 270); got != 0.25 {
		t.Fatalf("副屏 y=270 比例应为 0.25, got %.3f", got)
	}
}

func TestSlaveRelMoveAndLeave(t *testing.T) {
	inj := newFakeInjector()
	_, port := newTestService(t, inj)

	tm := dialMaster(t, port, "tok")
	if _, err := tm.recv(); err != nil {
		t.Fatal(err)
	}
	tm.send(wireMsg{T: "enter", Dir: "right"})
	waitFor(t, func() bool { return inj.absCount() > 0 }, "入口注入")

	// 相对移动
	tm.send(wireMsg{T: "move", DX: 100, DY: 10})
	waitFor(t, func() bool { return inj.relCount() > 0 }, "相对注入")
	if r := inj.rels[len(inj.rels)-1]; r[0] != 100 || r[1] != 10 {
		t.Fatalf("相对位移错误: got %v", r)
	}

	// 深入 50px（武装）
	tm.send(wireMsg{T: "move", DX: 50})
	waitFor(t, func() bool { return inj.relCount() >= 2 }, "第二次注入")

	// 推回共享边缘：切回触发区 = 暴露左边缘（副屏 x=-1920）
	inj.setCursor(-1920+edgeBand, 540) // 光标贴在副屏左边缘
	tm.send(wireMsg{T: "move", DX: -10})
	tm.send(wireMsg{T: "move", DX: -12})
	m, err := tm.recv()
	if err != nil {
		t.Fatalf("未收到 leave: %v", err)
	}
	if m.T != "leave" {
		t.Fatalf("应答应为 leave: %+v", m)
	}
}

func TestNoLeaveBeforeArmed(t *testing.T) {
	inj := newFakeInjector()
	_, port := newTestService(t, inj)

	tm := dialMaster(t, port, "tok")
	if _, err := tm.recv(); err != nil {
		t.Fatal(err)
	}
	tm.send(wireMsg{T: "enter", Dir: "right"})
	waitFor(t, func() bool { return inj.absCount() > 0 }, "入口注入")

	// 从未深入屏幕（maxDist < armPixels），贴边推动不应触发切回
	tm.send(wireMsg{T: "move", DX: -5})
	tm.send(wireMsg{T: "move", DX: -20})
	time.Sleep(200 * time.Millisecond)
	_ = tm.conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if _, err := tm.recv(); err == nil {
		t.Fatal("未武装时不应触发 leave")
	} else if err != io.EOF && !isTimeout(err) {
		t.Fatalf("意外错误: %v", err)
	}
}

func TestSlaveAuthReject(t *testing.T) {
	inj := newFakeInjector()
	_, port := newTestService(t, inj)
	tm := dialMaster(t, port, "WRONG")
	m, err := tm.recv()
	if err != nil || m.T != "error" || m.Msg != "unauthorized" {
		t.Fatalf("错误令牌应被拒绝: %+v %v", m, err)
	}
	if inj.relCount() != 0 {
		t.Fatal("被拒后不应有注入")
	}
}

func TestSlaveBusyReject(t *testing.T) {
	inj := newFakeInjector()
	_, port := newTestService(t, inj)

	tm1 := dialMaster(t, port, "tok")
	if _, err := tm1.recv(); err != nil {
		t.Fatal(err)
	}
	tm2 := dialMaster(t, port, "tok")
	m, err := tm2.recv()
	if err != nil || m.T != "error" || m.Msg != "busy" {
		t.Fatalf("第二个主控应被拒绝: %+v %v", m, err)
	}
}

// TestSlaveWatchdogRelease：主控机失联后应自动释放。
func TestSlaveWatchdogRelease(t *testing.T) {
	inj := newFakeInjector()
	_, port := newTestService(t, inj)

	tm := dialMaster(t, port, "tok")
	if _, err := tm.recv(); err != nil {
		t.Fatal(err)
	}
	tm.send(wireMsg{T: "enter", Dir: "right"})
	_ = tm.conn.SetReadDeadline(time.Now().Add(8 * time.Second))
	m, err := tm.recv()
	if err != nil || m.T != "leave" {
		t.Fatalf("看门狗应发送 leave: %+v %v", m, err)
	}
}

// 回归：OnMouseMove 在钩子线程上调用，绝不能死锁。
// 曾因持有 s.mu 时再锁 s.mu（显示器缓存）卡死钩子线程，导致全系统鼠标卡顿。
func TestOnMouseMoveLocalNoDeadlock(t *testing.T) {
	inj := newFakeInjector()
	store := discovery.NewStore("self")
	svc := NewService(Config{Port: 47899, Right: "GHOST", SelfID: "S-ID"}, "S", store, 12*time.Second, inj)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			svc.OnMouseMove(3, 0) // 触发本地边缘检测路径（曾死锁）
			svc.OnMouseMove(-1, 0)
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("OnMouseMove 死锁")
	}
}

// 被控期间本机物理输入应立即结束会话。
func TestLocalActivityEndsSession(t *testing.T) {
	inj := newFakeInjector()
	svc, port := newTestService(t, inj)

	tm := dialMaster(t, port, "tok")
	if _, err := tm.recv(); err != nil {
		t.Fatal(err)
	}
	tm.send(wireMsg{T: "enter", Dir: "right"})
	waitFor(t, func() bool { return inj.absCount() > 0 }, "入口注入")

	svc.OnLocalActivity() // 本机物理输入
	m, err := tm.recv()
	if err != nil || m.T != "leave" {
		t.Fatalf("本机物理输入应触发 leave: %+v %v", m, err)
	}
}

// 主控端 15s 读超时依赖被控端的 pong 应答：ping → pong 必须工作。
func TestSlavePongKeepsAlive(t *testing.T) {
	inj := newFakeInjector()
	_, port := newTestService(t, inj)

	tm := dialMaster(t, port, "tok")
	if _, err := tm.recv(); err != nil {
		t.Fatal(err)
	}
	tm.send(wireMsg{T: "enter", Dir: "right"})
	waitFor(t, func() bool { return inj.absCount() > 0 }, "入口注入")

	tm.send(wireMsg{T: "ping"})
	_ = tm.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	m, err := tm.recv()
	if err != nil || m.T != "pong" {
		t.Fatalf("ping 应得到 pong: %+v %v", m, err)
	}
}

// 回归：连接建立时设置的写截止时间过期后，pong 仍应能写出
// （曾因 SetDeadline 同时设置写截止时间，5s 后所有写入立刻 i/o timeout）。
func TestPongAfterInitialWriteDeadlineExpired(t *testing.T) {
	inj := newFakeInjector()
	_, port := newTestService(t, inj)

	tm := dialMaster(t, port, "tok")
	if _, err := tm.recv(); err != nil {
		t.Fatal(err)
	}
	tm.send(wireMsg{T: "enter", Dir: "right"})
	waitFor(t, func() bool { return inj.absCount() > 0 }, "入口注入")

	// 期间持续 ping 保活（真实场景主控 1s 一次），越过最初的 5s 截止窗口
	for i := 0; i < 4; i++ {
		time.Sleep(2 * time.Second)
		tm.send(wireMsg{T: "ping"})
		_ = tm.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		m, err := tm.recv()
		if err != nil || m.T != "pong" {
			t.Fatalf("第 %d 次 ping 应得到 pong: %+v %v", i+1, m, err)
		}
	}
}

// 回归：贴边推动时的事件级抖动（±px 交替）不允许把累积整笔清零——
// 高回报率鼠标贴边斜推时曾被"反向一笔清零"卡住，表现为边缘时灵时不灵。
func TestEdgePushSurvivesJitter(t *testing.T) {
	inj := newFakeInjector()
	svc := NewService(Config{Port: 47899, Right: "GHOST", SelfID: "S-ID"},
		"S", discovery.NewStore("self"), 12*time.Second, inj)
	// 主屏右边缘暴露：光标贴在 (1918,540)，以 +4/-1 交替抖动持续向外推
	inj.setCursor(1918, 540)
	for i := 0; i < 8; i++ {
		svc.OnMouseMove(4, 0)
		svc.OnMouseMove(-1, 0)
	}
	svc.mu.Lock()
	defer svc.mu.Unlock()
	if !time.Now().Before(svc.attemptUntil) {
		t.Fatalf("净推动 16px（含抖动抵消）应触发切换尝试, accum=%d", svc.pushAccum)
	}
}

// 回归：触发一次后的冷却/尝试窗口内不应立刻重复触发；
// 尝试窗口与成功冷却一致（200ms 防抖）。
func TestEdgePushCooldownGate(t *testing.T) {
	inj := newFakeInjector()
	svc := NewService(Config{Port: 47899, Right: "GHOST", SelfID: "S-ID"},
		"S", discovery.NewStore("self"), 12*time.Second, inj)
	inj.setCursor(1918, 540)
	svc.OnMouseMove(20, 0) // 一次推满阈值，触发尝试窗口
	svc.mu.Lock()
	until := svc.attemptUntil
	svc.mu.Unlock()
	if !time.Now().Before(until) {
		t.Fatal("触发后应进入尝试窗口")
	}
	if d := time.Until(until); d > 300*time.Millisecond {
		t.Fatalf("尝试窗口应为 200ms 量级, got %v", d)
	}
	svc.OnMouseMove(20, 0) // 窗口内的再次推动应被忽略
	svc.mu.Lock()
	until2 := svc.attemptUntil
	svc.mu.Unlock()
	if !until2.Equal(until) {
		t.Fatal("尝试窗口内的推动不应重置窗口")
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", what)
}

// 热更新：UpdateTunables 写入后各读取路径（move 合拍/reflow 节拍）立即取到
// 新值，无需重启；默认与哨兵归一化正确。
func TestUpdateTunablesHotApply(t *testing.T) {
	inj := newFakeInjector()
	svc, _ := newTestService(t, inj) // 默认 reflow 关闭（-1）

	if got := svc.currentMoveInterval(); got != DefaultMoveInterval {
		t.Fatalf("默认合拍应为 %v, got %v", DefaultMoveInterval, got)
	}
	if got := svc.currentReflowStep(); got != -1 {
		t.Fatalf("默认重排应为 -1（newTestService 关闭）, got %v", got)
	}

	svc.UpdateTunables(16*time.Millisecond, 5*time.Millisecond, 150)
	if got := svc.currentMoveInterval(); got != 16*time.Millisecond {
		t.Fatalf("热更合拍未生效: got %v", got)
	}
	if got := svc.currentReflowStep(); got != 5*time.Millisecond {
		t.Fatalf("热更重排未生效: got %v", got)
	}
	svc.mu.Lock()
	if svc.cfg.SpeedPercent != 150 {
		t.Fatalf("速度微调未生效: got %d", svc.cfg.SpeedPercent)
	}
	svc.mu.Unlock()

	// 哨兵与默认归一：<=0 视为默认，reflow 的 -1 表示关闭（保持负值）
	svc.UpdateTunables(0, -1, 0)
	if got := svc.currentMoveInterval(); got != DefaultMoveInterval {
		t.Fatalf("move=0 应归一为默认, got %v", got)
	}
	if got := svc.currentReflowStep(); got != -1 {
		t.Fatalf("reflow=-1 应保持关闭语义, got %v", got)
	}
	svc.mu.Lock()
	if svc.cfg.SpeedPercent != 100 {
		t.Fatalf("speedPct=0 应归一为 100, got %d", svc.cfg.SpeedPercent)
	}
	svc.mu.Unlock()
}

// 跨分辨率与实际设置缩放比静态计算测试：
func TestCalcStaticGain(t *testing.T) {
	// 1. 同分辨率同缩放比 (1080p 1.0x -> 1080p 1.0x) -> 1.0x
	gx, gy := calcStaticGain(1920, 1080, 1.0, 1920, 1080, 1.0, 100)
	if gx != 1.0 || gy != 1.0 {
		t.Fatalf("同分辨率基准应为 1.0: got gx=%.4f, gy=%.4f", gx, gy)
	}

	// 2. ThinkBook (2880x1800 200%缩放) -> 1080p (1920x1080 100%缩放):
	// Lm_x = 2880/2.0 = 1440, gx = 1920/1440 = 1.3333...
	// Lm_y = 1800/2.0 = 900, gy = 1080/900 = 1.2000
	gx, gy = calcStaticGain(2880, 1800, 2.0, 1920, 1080, 1.0, 100)
	if mathAbs(gx-1.3333) > 0.001 || mathAbs(gy-1.2) > 0.001 {
		t.Fatalf("高分屏 200%% 补偿计算错误: got gx=%.4f, gy=%.4f", gx, gy)
	}

	// 3. ThinkBook (2880x1800 200%缩放) -> 2K 屏 (2560x1440 125%缩放):
	// Lm_x = 1440, gx = 2560/1440 = 1.7777...
	// Lm_y = 900, gy = 1440/900 = 1.6000
	gx, gy = calcStaticGain(2880, 1800, 2.0, 2560, 1440, 1.25, 100)
	if mathAbs(gx-1.7778) > 0.001 || mathAbs(gy-1.6) > 0.001 {
		t.Fatalf("2K 屏补偿计算错误: got gx=%.4f, gy=%.4f", gx, gy)
	}

	// 4. 用户面板微调系数 150%: gx = 1.3333 * 1.5 = 2.0
	gx, gy = calcStaticGain(2880, 1800, 2.0, 1920, 1080, 1.0, 150)
	if mathAbs(gx-2.0) > 0.001 || mathAbs(gy-1.8) > 0.001 {
		t.Fatalf("用户微调系数叠加错误: got gx=%.4f, gy=%.4f", gx, gy)
	}
}

func mathAbs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// 相对移动 1:1 硬件物理位移传递测试：
// 确保不施加动态 EMA 或漂移系数，输入 dx, dy 守恒直接发出。
func TestMasterRelativeMove1to1(t *testing.T) {
	inj := newFakeInjector()
	svc, _ := newTestService(t, inj)
	sess := &masterSession{
		abs:    false,
		dir:    "right",
		gainX:  1,
		gainY:  1,
		sendCh: make(chan wireMsg, 16),
		stop:   make(chan struct{}),
	}
	svc.mu.Lock()
	svc.master = sess
	svc.cfg.MoveInterval = 0 // 立即发送
	svc.mu.Unlock()

	svc.OnMouseMove(15, -27)
	msg := <-sess.sendCh
	if msg.T != "move" || msg.DX != 15 || msg.DY != -27 {
		t.Fatalf("相对移动应 1:1 保持原值: got %+v, want DX=15, DY=-27", msg)
	}

	svc.OnMouseMove(-100, 50)
	msg = <-sess.sendCh
	if msg.T != "move" || msg.DX != -100 || msg.DY != 50 {
		t.Fatalf("相对移动应 1:1 保持原值: got %+v, want DX=-100, DY=50", msg)
	}
}

// 相对移动静态增益与亚像素余数结转测试：
func TestMasterRelativeMoveWithGainAndSubpixel(t *testing.T) {
	inj := newFakeInjector()
	svc, _ := newTestService(t, inj)
	sess := &masterSession{
		abs:    false,
		dir:    "right",
		gainX:  1.3333333333,
		gainY:  1.2,
		sendCh: make(chan wireMsg, 16),
		stop:   make(chan struct{}),
	}
	svc.mu.Lock()
	svc.master = sess
	svc.cfg.MoveInterval = 0 // 立即发送
	svc.mu.Unlock()

	// 第一次：dx=1, dy=1 -> fx = 1.333, fy = 1.2 -> ddx = 1, ddy = 1, resX = 0.333, resY = 0.2
	svc.OnMouseMove(1, 1)
	msg := <-sess.sendCh
	if msg.DX != 1 || msg.DY != 1 {
		t.Fatalf("首步四舍五入: got DX=%d, DY=%d", msg.DX, msg.DY)
	}

	// 第二次：dx=1, dy=1 -> fx = 1.333 + 0.333 = 1.666 -> ddx = 2! fy = 1.2 + 0.2 = 1.4 -> ddy = 1
	svc.OnMouseMove(1, 1)
	msg = <-sess.sendCh
	if msg.DX != 2 || msg.DY != 1 {
		t.Fatalf("亚像素结转累加: got DX=%d, DY=%d", msg.DX, msg.DY)
	}
}

// Universal 0..65535 模型：MWB 架构下直接在 0..65535 空间跟踪光标。
// 贴边时钳制在 0..65535 内，反向一推立即生效（无死区/无倒带）。
// 越过共享边缘超过阈值直接触发 Master 会话结束（切回）。
func TestMasterUMoveTrackingAndEdgeSwitchback(t *testing.T) {
	t.Run("右向会话_贴边钳制与反向即刻生效", func(t *testing.T) {
		inj := newFakeInjector()
		svc, _ := newTestService(t, inj)
		sess := &masterSession{
			abs: true, dir: "right",
			normX: 0, normY: 32767,
			anchorW: 2880, anchorH: 1800,
			sendCh: make(chan wireMsg, 16), stop: make(chan struct{}),
		}
		svc.mu.Lock()
		svc.master = sess
		svc.cfg.MoveInterval = 0
		svc.mu.Unlock()

		// 向右移动 100px: normX 增加 100 * 65535 / 2880 = 2275.5 -> 2276
		svc.OnMouseMove(100, 0)
		msg := <-sess.sendCh
		if msg.T != "umove" || msg.AX != 2276 {
			t.Fatalf("normX 跟踪错误: got AX=%d, want 2276", msg.AX)
		}

		// 猛推右边缘：钳制在 65535，不发散
		svc.OnMouseMove(100000, 0)
		msg = <-sess.sendCh
		if msg.AX != 65535 {
			t.Fatalf("右边缘应钳制在 65535: got AX=%d", msg.AX)
		}

		// 反向立即响应：直接从 65535 回退，绝不倒带 100000px
		svc.OnMouseMove(-100, 0)
		msg = <-sess.sendCh
		if msg.AX != 65535-2276 {
			t.Fatalf("反向应立即生效无死区: got AX=%d, want %d", msg.AX, 65535-2276)
		}
	})

	t.Run("右向会话_推回共享边缘触发切回", func(t *testing.T) {
		inj := newFakeInjector()
		svc, _ := newTestService(t, inj)
		sess := &masterSession{
			abs: true, dir: "right",
			normX: 100, normY: 32767,
			anchorW: 2880, anchorH: 1800,
			sendCh: make(chan wireMsg, 16), stop: make(chan struct{}),
		}
		svc.mu.Lock()
		svc.master = sess
		svc.cfg.MoveInterval = 0
		svc.mu.Unlock()

		// 向左推回越过 normX=0：dx=-100 -> delta = -2275.5 -> normX = -2175.5 < -500
		// 超过 umovePushThreshold 阈值，立即触发 endMasterSession
		svc.OnMouseMove(-100, 0)

		select {
		case <-sess.stop:
			// 成功切回
		case <-time.After(500 * time.Millisecond):
			t.Fatal("推回共享边缘应立即触发 Master 会话切回")
		}

		svc.mu.Lock()
		activeMaster := svc.master
		svc.mu.Unlock()
		if activeMaster != nil {
			t.Fatal("切回后 svc.master 应为 nil")
		}
	})

	t.Run("左向会话_推回共享边缘触发切回", func(t *testing.T) {
		inj := newFakeInjector()
		svc, _ := newTestService(t, inj)
		sess := &masterSession{
			abs: true, dir: "left",
			normX: 65435, normY: 32767,
			anchorW: 2880, anchorH: 1800,
			sendCh: make(chan wireMsg, 16), stop: make(chan struct{}),
		}
		svc.mu.Lock()
		svc.master = sess
		svc.cfg.MoveInterval = 0
		svc.mu.Unlock()

		// 向右推回越过 normX=65535：dx=100 -> delta = +2275.5 -> normX > 65535+500
		svc.OnMouseMove(100, 0)

		select {
		case <-sess.stop:
			// 成功切回
		case <-time.After(500 * time.Millisecond):
			t.Fatal("推回共享边缘应立即触发 Master 会话切回")
		}

		svc.mu.Lock()
		activeMaster := svc.master
		svc.mu.Unlock()
		if activeMaster != nil {
			t.Fatal("切回后 svc.master 应为 nil")
		}
	})
}

// 归一化基准的取宽高：返回光标所属显示器的宽高（fake 两屏均 1920x1080）。
func TestMonitorWHAt(t *testing.T) {
	inj := newFakeInjector()
	svc, _ := newTestService(t, inj)
	if w, h := svc.monitorWHAt(960, 540); w != 1920 || h != 1080 { // 主屏
		t.Fatalf("主屏宽高应为 1920x1080, got %vx%v", w, h)
	}
	if w, h := svc.monitorWHAt(-960, 540); w != 1920 || h != 1080 { // 副屏
		t.Fatalf("副屏宽高应为 1920x1080, got %vx%v", w, h)
	}
	if w, h := svc.monitorWHAt(99999, 99999); w != 1920 || h != 1080 { // 无匹配屏退回主屏
		t.Fatalf("无匹配屏应退回主屏, got %vx%v", w, h)
	}
}

// 重排：整包累计位移被拆成多个更小的步注入，最终位移精确守恒。
func TestReflowSplitsAndConserves(t *testing.T) {
	inj := newFakeInjector()
	_, port := newTestServiceReflow(t, inj, 8*time.Millisecond)

	tm := dialMaster(t, port, "tok")
	if _, err := tm.recv(); err != nil {
		t.Fatal(err)
	}
	tm.send(wireMsg{T: "enter", Dir: "right", Y: 0.5})
	waitFor(t, func() bool { return inj.absCount() > 0 }, "入口注入")

	tm.send(wireMsg{T: "move", DX: 100, DY: 10})
	waitFor(t, func() bool {
		sx, sy := inj.relSum()
		return sx == 100 && sy == 10
	}, "位移追平")

	if inj.relCount() <= 1 {
		t.Fatalf("单包 100px 应被拆成多步，got relCount=%d", inj.relCount())
	}
	if m := inj.maxRelStep(); m >= 100 {
		t.Fatalf("每步应严格小于整包位移，got maxRelStep=%d", m)
	}
}

// 重排：成批到达的多次移动合并后按节拍排空，位移总和守恒。
func TestReflowBatchesConserve(t *testing.T) {
	inj := newFakeInjector()
	_, port := newTestServiceReflow(t, inj, 4*time.Millisecond)

	tm := dialMaster(t, port, "tok")
	if _, err := tm.recv(); err != nil {
		t.Fatal(err)
	}
	tm.send(wireMsg{T: "enter", Dir: "right", Y: 0.5})
	waitFor(t, func() bool { return inj.absCount() > 0 }, "入口注入")

	// 连续快速推 4 个包（模拟主控合拍 + 网络成批），每包 10px
	for i := 0; i < 4; i++ {
		if err := tm.send(wireMsg{T: "move", DX: 10, DY: -5}); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, func() bool {
		sx, sy := inj.relSum()
		return sx == 40 && sy == -20
	}, "批量位移追平")
}

// 会话结束时残余 pending 必须被 flush，位移不丢失。
func TestReflowFlushOnSessionEnd(t *testing.T) {
	inj := newFakeInjector()
	_, port := newTestServiceReflow(t, inj, 20*time.Millisecond) // 节拍放慢，确保结束时尚有 pending

	tm := dialMaster(t, port, "tok")
	if _, err := tm.recv(); err != nil {
		t.Fatal(err)
	}
	tm.send(wireMsg{T: "enter", Dir: "right", Y: 0.5})
	waitFor(t, func() bool { return inj.absCount() > 0 }, "入口注入")

	tm.send(wireMsg{T: "move", DX: 57, DY: 33})
	// 立即断开（不等节拍排空），触发 handleSlaveConn 退出 → stopReflow flush
	tm.conn.Close()
	waitFor(t, func() bool {
		sx, sy := inj.relSum()
		return sx == 57 && sy == 33
	}, "结束时残余位移 flush")
}

// ---------- B4：Universal 0..65535 绝对注入 ----------

// 握手能力协商（B4）：主控在 hello 声明 abs，被控在 ok 回执确认。
// 握手能力协商（B4）：主控在 hello 声明 abs，被控在 ok 回执确认。
// 当被控端为多显示器时，自动协商 Abs=false 回退相对通道以支持副屏漫游；
// 单显示器且双方支持时回执 Abs=true；任一侧缺省（旧版本）都必须退回相对位移通道。
func TestHandshakeNegotiatesUniversal(t *testing.T) {
	t.Run("双方支持且单屏", func(t *testing.T) {
		inj := newFakeInjector()
		inj.monsOverride = []input.Rect{{X: 0, Y: 0, W: 1920, H: 1080, Primary: true}}
		_, port := newTestService(t, inj)
		tm := dialMasterCap(t, port, "tok", true)
		m, err := tm.recv()
		if err != nil {
			t.Fatal(err)
		}
		if m.T != "ok" || !m.Abs {
			t.Fatalf("单屏且双方支持时 ok 应回执 Abs=true: %+v", m)
		}
	})
	t.Run("被控多屏自动降级为相对通道", func(t *testing.T) {
		_, port := newTestService(t, newFakeInjector()) // 默认双屏：副屏+主屏
		tm := dialMasterCap(t, port, "tok", true)
		m, err := tm.recv()
		if err != nil {
			t.Fatal(err)
		}
		if m.T != "ok" || m.Abs {
			t.Fatalf("被控多屏时 ok 应回执 Abs=false 以启用相对通道: %+v", m)
		}
	})
	t.Run("旧主控", func(t *testing.T) {
		_, port := newTestService(t, newFakeInjector())
		tm := dialMaster(t, port, "tok") // 不带 abs 字段（旧版本形态）
		m, err := tm.recv()
		if err != nil {
			t.Fatal(err)
		}
		if m.T != "ok" || m.Abs {
			t.Fatalf("旧主控时 ok 不应带 Abs: %+v", m)
		}
	})
}

// 被控端在 enter 后注入光标到入口点，且不发送 band 消息（MWB 原生模型无需 band）
func TestSlaveEnterPositionsCursor(t *testing.T) {
	inj := newFakeInjector()
	_, port := newTestService(t, inj)
	tm := dialMasterCap(t, port, "tok", true)
	if _, err := tm.recv(); err != nil { // ok
		t.Fatal(err)
	}
	tm.send(wireMsg{T: "enter", Dir: "right", Y: 0.5})
	waitFor(t, func() bool { return inj.absCount() > 0 }, "入口注入")
	x, y := inj.lastAbs()
	if x != 2 || y != 540 {
		t.Fatalf("入口注入坐标错误: got (%d,%d), want (2,540)", x, y)
	}
	// 不应发送 band 消息
	_ = tm.conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if m, err := tm.recv(); err == nil {
		t.Fatalf("enter 后不应发送任何消息: got %+v", m)
	}
}

// 真机事故回归：被控端存在带 Primary 标志的 1x1 幽灵显示器时，绝不能选它作
// 入口——否则光标被 MoveAbs 钉在桌面左上角完全无法移动。
// 过滤后应落到真实工作屏。
func TestSlaveEntrySkipsDegeneratePrimary(t *testing.T) {
	inj := newFakeInjector()
	inj.mu.Lock()
	inj.monsOverride = []input.Rect{
		{X: 0, Y: 0, W: 1, H: 1, Primary: true}, // 幽灵占位屏（却是 Primary）
		{X: 0, Y: 0, W: 1920, H: 1080},          // 真实工作屏
	}
	inj.mu.Unlock()
	_, port := newTestService(t, inj)
	tm := dialMasterCap(t, port, "tok", true)
	if _, err := tm.recv(); err != nil { // ok
		t.Fatal(err)
	}
	tm.send(wireMsg{T: "enter", Dir: "right", Y: 0.5})
	waitFor(t, func() bool { return inj.absCount() > 0 }, "入口注入")
	x, y := inj.lastAbs()
	if x != 2 || y != 540 {
		t.Fatalf("入口应落在真实工作屏共享边缘: got (%d,%d), want (2,540)", x, y)
	}
}

// 被控端在未协商 abs 的会话里必须忽略 umove（协议异常保护）。
func TestSlaveIgnoresUMoveWithoutNegotiation(t *testing.T) {
	inj := newFakeInjector()
	inj.monsOverride = []input.Rect{{X: 0, Y: 0, W: 1920, H: 1080, Primary: true}}
	_, port := newTestService(t, inj)
	tm := dialMaster(t, port, "tok") // 未声明 abs
	if _, err := tm.recv(); err != nil {
		t.Fatal(err)
	}
	tm.send(wireMsg{T: "enter", Dir: "right", Y: 0.5})
	waitFor(t, func() bool { return inj.absCount() > 0 }, "入口注入")
	countBefore := inj.absCount()
	tm.send(wireMsg{T: "umove", AX: 34133, AY: 0})
	time.Sleep(100 * time.Millisecond)
	if inj.absCount() != countBefore {
		t.Fatalf("未协商 abs 时 umove 必须被忽略, absCount=%d, want %d", inj.absCount(), countBefore)
	}
}

// umove 直注映射（单屏环境下）：
// 验证 0..65535 屏幕绝对归一化坐标精准映射到目标屏幕像素并调用 MoveAbs (SetCursorPos)
func TestSlaveUMoveDirectMapping(t *testing.T) {
	inj := newFakeInjector()
	inj.monsOverride = []input.Rect{{X: 0, Y: 0, W: 1920, H: 1080, Primary: true}}
	_, port := newTestService(t, inj)
	tm := dialMasterCap(t, port, "tok", true)
	if _, err := tm.recv(); err != nil {
		t.Fatal(err)
	}
	tm.send(wireMsg{T: "enter", Dir: "right", Y: 0.5})
	waitFor(t, func() bool { return inj.absCount() > 0 }, "入口注入")

	// 目标入口屏为 (0,0 1920x1080)
	// (0, 0) -> (0, 0)
	tm.send(wireMsg{T: "umove", AX: 0, AY: 0})
	waitFor(t, func() bool {
		x, y := inj.lastPixel()
		return x == 0 && y == 0
	}, "umove (0,0)")

	// (32767, 32767) -> (959, 539)
	tm.send(wireMsg{T: "umove", AX: 32767, AY: 32767})
	waitFor(t, func() bool {
		x, y := inj.lastPixel()
		return x == 959 && y == 539
	}, "umove (959,539)")

	// (65535, 65535) -> (1919, 1079)
	tm.send(wireMsg{T: "umove", AX: 65535, AY: 65535})
	waitFor(t, func() bool {
		x, y := inj.lastPixel()
		return x == 1919 && y == 1079
	}, "umove (1919,1079)")
}

// 验证在被控双屏环境下，相对 move 能够自然跨入副屏（X < 0），光标不被锁死在主屏。
func TestSlaveMoveCrossesToSecondaryMonitor(t *testing.T) {
	inj := newFakeInjector() // 默认双屏：副屏(-1920,0) + 主屏(0,0)
	_, port := newTestService(t, inj)
	tm := dialMaster(t, port, "tok")
	if _, err := tm.recv(); err != nil {
		t.Fatal(err)
	}
	tm.send(wireMsg{T: "enter", Dir: "right", Y: 0.5})
	waitFor(t, func() bool { return inj.absCount() > 0 }, "入口注入")
	x, y := inj.CursorPos()
	if x != 2 || y != 540 {
		t.Fatalf("入口坐标错误: got (%d,%d), want (2,540)", x, y)
	}

	// 向左移动 100px: 跨过 x=0 进入副屏 x=-98
	tm.send(wireMsg{T: "move", DX: -100, DY: 0})
	waitFor(t, func() bool {
		cx, _ := inj.CursorPos()
		return cx < 0
	}, "跨入副屏")

	cx, cy := inj.CursorPos()
	if cx >= 0 {
		t.Fatalf("光标应已进入副屏（X < 0）: got (%d,%d)", cx, cy)
	}
	if cy != 540 {
		t.Fatalf("Y 轴坐标不应发生偏移: got %d, want 540", cy)
	}

	// 继续深入副屏
	tm.send(wireMsg{T: "move", DX: -500, DY: 0})
	waitFor(t, func() bool {
		cx, _ := inj.CursorPos()
		return cx <= -500
	}, "深入副屏")
}
