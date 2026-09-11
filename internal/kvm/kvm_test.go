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
	rels    [][2]int
	clicks  []string
	wheels  []int32
	keys    []uint32
	cursorX int
	cursorY int
	speed   int // MouseSpeed 返回值（0=不补偿，保持既有测试语义）
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
	conn, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", port), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	bw := bufio.NewWriter(conn)
	b, _ := json.Marshal(wireMsg{T: "hello", Token: token, ID: "MASTER-ID", Name: "MASTER"})
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
// 失败后的重试锁定与冷却一致（1.5s），不再锁死 3 秒。
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
	if d := time.Until(until); d > 1600*time.Millisecond {
		t.Fatalf("尝试窗口应为 1.5s 量级, got %v", d)
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

// 热更新：UpdateTunables 写入后各读取路径（move 合拍/reflow 节拍/速度系数）
// 立即取到新值，无需重启；默认与哨兵归一化正确。
func TestUpdateTunablesHotApply(t *testing.T) {
	inj := newFakeInjector()
	svc, _ := newTestService(t, inj) // 默认 reflow 关闭（-1）

	if got := svc.currentSpeedPercent(); got != 100 { // 默认 100=不补偿
		t.Fatalf("默认系数应为 100, got %d", got)
	}
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
	if got := svc.currentSpeedPercent(); got != 150 {
		t.Fatalf("热更系数未生效: got %d", got)
	}
	// 哨兵与默认归一：<=0 视为默认，reflow 的 -1 表示关闭（保持负值）
	svc.UpdateTunables(0, -1, 0)
	if got := svc.currentMoveInterval(); got != DefaultMoveInterval {
		t.Fatalf("move=0 应归一为默认, got %v", got)
	}
	if got := svc.currentReflowStep(); got != -1 {
		t.Fatalf("reflow=-1 应保持关闭语义, got %v", got)
	}
	if got := svc.currentSpeedPercent(); got != 100 {
		t.Fatalf("speed<=0 应归一为 100, got %d", got)
	}
}

// 手调系数：speedGainPct = 百分比/100；<=0 视为 100（1.0x，不补偿）。
func TestSpeedGainPct(t *testing.T) {
	cases := []struct {
		pct  int
		want float64
	}{
		{0, 1},      // 未配置 → 1.0
		{-5, 1},     // 非法 → 1.0
		{100, 1},    // 基准
		{125, 1.25}, // 偏慢上调
		{80, 0.8},   // 偏快下调
	}
	for _, c := range cases {
		if got := speedGainPct(c.pct); got != c.want {
			t.Fatalf("pct=%d: got %.2f, want %.2f", c.pct, got, c.want)
		}
	}
}

// 缩放补偿的取宽：返回光标所属显示器的物理宽（fake 两屏均 1920 宽）。
func TestMonitorWidthAt(t *testing.T) {
	inj := newFakeInjector()
	svc, _ := newTestService(t, inj)
	if got := svc.monitorWidthAt(960, 540); got != 1920 { // 主屏
		t.Fatalf("主屏宽应为 1920, got %d", got)
	}
	if got := svc.monitorWidthAt(-960, 540); got != 1920 { // 副屏
		t.Fatalf("副屏宽应为 1920, got %d", got)
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
