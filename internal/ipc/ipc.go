//go:build windows

// Package ipc 提供本机进程间通信：命名管道 + 单次 JSON 请求/响应。
//
// 用途：
//   - 资源管理器右键「用 copywhere 发送」把路径交给已运行实例；
//   - 第二次双击 exe 时唤起已有窗口。
//
// 选择命名管道而不是本地 HTTP：不需要端口、不需要令牌 URL、不会被浏览器/防火墙
// 策略影响；管道名按登录会话隔离（同一台机器的不同用户互不可见）。
package ipc

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	maxMessage = 1 << 20 // 单条请求上限（路径列表足够用）
	pipePrefix = `\\.\pipe\copywhere-`
)

// 命令名。
const (
	CmdPing     = "ping"
	CmdActivate = "activate"
	CmdSend     = "send"
)

// Command 是客户端发给运行中实例的请求。
type Command struct {
	Cmd   string   `json:"cmd"`
	Paths []string `json:"paths,omitempty"`
}

// reply 是服务端的响应。
type reply struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// ErrNotRunning 表示没有实例在监听（copywhere 没运行）。
var ErrNotRunning = errors.New("copywhere 未运行")

// PipeName 返回当前登录会话的管道名（不同会话互不干扰）。
func PipeName() string {
	var session uint32
	if err := windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &session); err != nil {
		session = 0
	}
	return fmt.Sprintf("%s%d", pipePrefix, session)
}

// Server 是监听中的管道服务端。
type Server struct {
	handle  windows.Handle
	handler func(Command) error

	stopped atomic.Bool
	wg      sync.WaitGroup
}

// Serve 创建管道并开始接受请求；handler 在后台 goroutine 中串行执行。
//
// 返回值一定非 nil（失败时先尝试清理）。调用方应在退出时 Close。
func Serve(handler func(Command) error) (*Server, error) {
	name, err := windows.UTF16PtrFromString(PipeName())
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateNamedPipe(
		name,
		windows.PIPE_ACCESS_DUPLEX,
		windows.PIPE_TYPE_MESSAGE|windows.PIPE_READMODE_MESSAGE|windows.PIPE_WAIT,
		1, // 单实例：请求串行处理，避免并发写配置/发送
		4096,
		maxMessage,
		0,
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("创建命名管道失败: %w", err)
	}
	s := &Server{handle: h, handler: handler}
	s.wg.Add(1)
	go s.loop()
	return s, nil
}

// Close 停止监听。
//
// 注意：ConnectNamedPipe 是阻塞调用，直接 CloseHandle 并不会可靠地唤醒它
// （实测会卡住），因此这里先"自连"一次把 accept 唤醒，再等循环退出并关闭句柄。
func (s *Server) Close() {
	if s == nil || s.stopped.Swap(true) {
		return
	}
	name, err := windows.UTF16PtrFromString(PipeName())
	if err == nil {
		if h, derr := windows.CreateFile(name,
			windows.GENERIC_READ|windows.GENERIC_WRITE,
			0, nil, windows.OPEN_EXISTING, 0, 0); derr == nil {
			_ = windows.CloseHandle(h) // 只为唤醒 accept，不发送任何数据
		}
	}
	s.wg.Wait()
	_ = windows.CloseHandle(s.handle)
}

// loop 逐个接受连接；每个连接一问一答后断开。
func (s *Server) loop() {
	defer s.wg.Done()
	for {
		if s.stopped.Load() {
			return
		}
		err := windows.ConnectNamedPipe(s.handle, nil)
		if err != nil && !errors.Is(err, windows.ERROR_PIPE_CONNECTED) {
			return
		}
		if s.stopped.Load() {
			_ = windows.DisconnectNamedPipe(s.handle)
			return
		}
		s.handleConn()
		_ = windows.DisconnectNamedPipe(s.handle)
	}
}

// handleConn 读取一条请求、执行、写回结果。
func (s *Server) handleConn() {
	buf := make([]byte, maxMessage)
	var n uint32
	if err := windows.ReadFile(s.handle, buf, &n, nil); err != nil || n == 0 {
		return // 客户端连上就断开：无需回应
	}
	var cmd Command
	if err := json.Unmarshal(buf[:n], &cmd); err != nil {
		s.writeReply(reply{Error: "请求不是合法 JSON: " + err.Error()})
		return
	}
	if err := s.handler(cmd); err != nil {
		s.writeReply(reply{Error: err.Error()})
		return
	}
	s.writeReply(reply{OK: true})
}

func (s *Server) writeReply(r reply) {
	raw, err := json.Marshal(r)
	if err != nil {
		return
	}
	var n uint32
	_ = windows.WriteFile(s.handle, raw, &n, nil)
}

// Send 把一条请求发给已运行实例并等待响应。
func Send(cmd Command) error {
	name, err := windows.UTF16PtrFromString(PipeName())
	if err != nil {
		return err
	}
	h, err := dial(name)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)

	raw, err := json.Marshal(cmd)
	if err != nil {
		return err
	}
	var n uint32
	if err := windows.WriteFile(h, raw, &n, nil); err != nil {
		return fmt.Errorf("发送请求失败: %w", err)
	}

	buf := make([]byte, 4096)
	var rn uint32
	if err := windows.ReadFile(h, buf, &rn, nil); err != nil {
		return fmt.Errorf("读取响应失败: %w", err)
	}
	var r reply
	if err := json.Unmarshal(buf[:rn], &r); err != nil {
		return fmt.Errorf("响应不是合法 JSON: %w", err)
	}
	if !r.OK {
		if r.Error == "" {
			return errors.New("请求被拒绝")
		}
		return errors.New(r.Error)
	}
	return nil
}

// dial 连接管道；服务端忙时短暂等待后重试一次。
func dial(name *uint16) (windows.Handle, error) {
	h, err := windows.CreateFile(name,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		0, nil, windows.OPEN_EXISTING, 0, 0)
	if errors.Is(err, windows.ERROR_PIPE_BUSY) {
		if werr := waitNamedPipe(name, 1000); werr == nil {
			h, err = windows.CreateFile(name,
				windows.GENERIC_READ|windows.GENERIC_WRITE,
				0, nil, windows.OPEN_EXISTING, 0, 0)
		}
	}
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PIPE_BUSY) {
			return 0, fmt.Errorf("%w（管道 %s 不可用）", ErrNotRunning, PipeName())
		}
		return 0, fmt.Errorf("连接命名管道失败: %w", err)
	}
	mode := uint32(windows.PIPE_READMODE_MESSAGE)
	if err := windows.SetNamedPipeHandleState(h, &mode, nil, nil); err != nil {
		_ = windows.CloseHandle(h)
		return 0, fmt.Errorf("设置管道读取模式失败: %w", err)
	}
	return h, nil
}

// Ping 探测是否有实例在运行。
func Ping() error { return Send(Command{Cmd: CmdPing}) }

// Activate 请求已有实例把窗口显示到前台。
func Activate() error { return Send(Command{Cmd: CmdActivate}) }

// SendPaths 请求已有实例发送这些本地路径。
func SendPaths(paths []string) error { return Send(Command{Cmd: CmdSend, Paths: paths}) }

// waitNamedPipe 是 kernel32!WaitNamedPipeW 的轻量封装（x/sys 未导出该函数）。
func waitNamedPipe(name *uint16, timeoutMs uint32) error {
	proc := windows.NewLazySystemDLL("kernel32.dll").NewProc("WaitNamedPipeW")
	r, _, err := proc.Call(uintptr(unsafe.Pointer(name)), uintptr(timeoutMs))
	if r == 0 {
		return err
	}
	return nil
}
