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
)

// fakeInjector 记录注入调用，模拟 1920x1080 单屏。
type fakeInjector struct {
	mu     sync.Mutex
	moves  [][2]int
	clicks []string
	wheels []int32
	keys   []uint32
	bounds [4]int
}

func newFakeInjector() *fakeInjector {
	return &fakeInjector{bounds: [4]int{0, 0, 1920, 1080}}
}

func (f *fakeInjector) MoveAbs(x, y int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.moves = append(f.moves, [2]int{x, y})
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
func (f *fakeInjector) ScreenBounds() (int, int, int, int) {
	return f.bounds[0], f.bounds[1], f.bounds[2], f.bounds[3]
}
func (f *fakeInjector) CursorPos() (int, int) { return 0, 0 }

func (f *fakeInjector) moveCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.moves)
}
func (f *fakeInjector) lastMove() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.moves[len(f.moves)-1][0], f.moves[len(f.moves)-1][1]
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
	if _, err := tm.conn.Write(append(b, '\n')); err != nil {
		return err
	}
	return nil
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

func TestSlaveHandshakeAuthInjection(t *testing.T) {
	inj := newFakeInjector()
	_, port := newTestService(t, inj)

	tm := dialMaster(t, port, "tok")
	resp, err := tm.recv()
	if err != nil || resp.T != "ok" {
		t.Fatalf("握手应答异常: %+v %v", resp, err)
	}

	// 进入：主控机从其右边缘切出（dir=right），入口在副机左边缘
	if err := tm.send(wireMsg{T: "enter", Dir: "right", Y: 0.5}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(leaveGrace + 100*time.Millisecond) // 越过宽限期

	// 移动到副机中部：m=1.5 → rel=0.5 → x=960
	tm.send(wireMsg{T: "move", X: 1.5, Y: 0.5})
	waitFor(t, func() bool { return inj.moveCount() > 0 }, "注入移动")
	if x, y := inj.lastMove(); x != 960 || y != 540 {
		t.Fatalf("注入坐标错误: got (%d,%d), want (960,540)", x, y)
	}

	// 按键/滚轮/点击
	tm.send(wireMsg{T: "btn", B: 1, Down: true})
	tm.send(wireMsg{T: "wheel", D: -120})
	tm.send(wireMsg{T: "key", VK: 0x41, Down: true})
	waitFor(t, func() bool {
		inj.mu.Lock()
		defer inj.mu.Unlock()
		return len(inj.clicks) > 0 && len(inj.wheels) > 0 && len(inj.keys) > 0
	}, "注入按键/滚轮/点击")
}

func TestSlaveLeaveOnEdgePush(t *testing.T) {
	inj := newFakeInjector()
	_, port := newTestService(t, inj)

	tm := dialMaster(t, port, "tok")
	if _, err := tm.recv(); err != nil { // ok
		t.Fatal(err)
	}
	tm.send(wireMsg{T: "enter", Dir: "right", Y: 0.5})
	time.Sleep(leaveGrace + 100*time.Millisecond)

	// 先深入副机屏幕（武装）
	tm.send(wireMsg{T: "move", X: 1.3, Y: 0.5})
	waitFor(t, func() bool { return inj.moveCount() > 0 }, "武装移动")

	// 向回推过共享边缘：r = m-1 < -leaveSoft，连续两次 → leave
	tm.send(wireMsg{T: "move", X: 0.99, Y: 0.5})
	tm.send(wireMsg{T: "move", X: 0.98, Y: 0.5})
	m, err := tm.recv()
	if err != nil {
		t.Fatalf("未收到 leave: %v", err)
	}
	if m.T != "leave" {
		t.Fatalf("应答应为 leave: %+v", m)
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
	if inj.moveCount() != 0 {
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
	tm.send(wireMsg{T: "enter", Dir: "right", Y: 0.5})
	// 不再发任何消息，等待读超时（5s）后 leave
	_ = tm.conn.SetReadDeadline(time.Now().Add(8 * time.Second))
	m, err := tm.recv()
	if err != nil || m.T != "leave" {
		t.Fatalf("看门狗应发送 leave: %+v %v", m, err)
	}
}

// TestNoLeaveDuringGrace：进入宽限期内推过边缘不应立即离开。
func TestNoLeaveDuringGrace(t *testing.T) {
	inj := newFakeInjector()
	_, port := newTestService(t, inj)

	tm := dialMaster(t, port, "tok")
	if _, err := tm.recv(); err != nil {
		t.Fatal(err)
	}
	tm.send(wireMsg{T: "enter", Dir: "right", Y: 0.5})
	tm.send(wireMsg{T: "move", X: 0.9, Y: 0.5})
	tm.send(wireMsg{T: "move", X: 0.8, Y: 0.5})
	time.Sleep(200 * time.Millisecond)
	_ = tm.conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if _, err := tm.recv(); err == nil {
		t.Fatal("宽限期内不应触发 leave")
	} else if err != io.EOF && !isTimeout(err) {
		// 期待读超时（宽限期内无 leave）
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
