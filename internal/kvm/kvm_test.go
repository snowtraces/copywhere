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
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	store := discovery.NewStore("self")
	svc := NewService(Config{Port: port}, "SLAVE", "tok", store, 12*time.Second, inj)
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
	b, _ := json.Marshal(wireMsg{T: "hello", Token: token, Name: "MASTER"})
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

func TestPickEntryMonitorPrefersPrimary(t *testing.T) {
	inj := newFakeInjector()
	mons := inj.Monitors()
	entry := pickEntryMonitor(mons, "right") // 主控机在我左侧 → 入口走暴露左边缘
	if !entry.Primary {
		t.Fatalf("dir=right 无暴露左边缘的主屏时应退回主屏, got %+v", entry)
	}
}

func TestSlaveEntryOnPrimarySharedEdge(t *testing.T) {
	inj := newFakeInjector()
	_, port := newTestService(t, inj)

	tm := dialMaster(t, port, "tok")
	if _, err := tm.recv(); err != nil { // ok
		t.Fatal(err)
	}
	// 主控机从其右边缘切出（dir=right）→ 我方入口在暴露左边缘；
	// 双屏布局中暴露左边缘是副屏（-1920），无主屏候选 → 退回主屏左边缘
	if err := tm.send(wireMsg{T: "enter", Dir: "right"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(inj.abs) > 0 }, "入口绝对注入")
	x, y := inj.lastAbs()
	if x != 0+entryMargin || y != 540 {
		t.Fatalf("入口坐标错误: got (%d,%d), want (2,540)", x, y)
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
	waitFor(t, func() bool { return len(inj.abs) > 0 }, "入口注入")

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
	waitFor(t, func() bool { return len(inj.abs) > 0 }, "入口注入")

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
		t.Fatalf("错误 token 应被拒绝: %+v %v", m, err)
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
	svc := NewService(Config{Port: 47899, Right: "GHOST"}, "S", "tok", store, 12*time.Second, inj)
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
