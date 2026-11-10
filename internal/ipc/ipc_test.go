//go:build windows

package ipc

import (
	"errors"
	"testing"
	"time"
)

func TestServeAndSend(t *testing.T) {
	got := make(chan Command, 4)
	srv, err := Serve(func(c Command) error {
		got <- c
		return nil
	})
	if err != nil {
		t.Fatalf("启动管道失败: %v", err)
	}
	defer srv.Close()

	if err := Send(Command{Cmd: CmdPing}); err != nil {
		t.Fatalf("ping 失败: %v", err)
	}
	if err := SendPaths([]string{`C:\tmp\a.txt`, `C:\tmp\b`}); err != nil {
		t.Fatalf("send 失败: %v", err)
	}

	deadline := time.After(3 * time.Second)
	var gotPing, gotSend bool
	for !(gotPing && gotSend) {
		select {
		case c := <-got:
			switch c.Cmd {
			case CmdPing:
				gotPing = true
			case CmdSend:
				gotSend = true
				if len(c.Paths) != 2 || c.Paths[0] != `C:\tmp\a.txt` {
					t.Fatalf("路径丢失: %v", c.Paths)
				}
			}
		case <-deadline:
			t.Fatalf("未收到命令: ping=%v send=%v", gotPing, gotSend)
		}
	}
}

func TestHandlerErrorReturnedToClient(t *testing.T) {
	srv, err := Serve(func(c Command) error {
		if c.Cmd == "boom" {
			return errors.New("处理失败")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("启动管道失败: %v", err)
	}
	defer srv.Close()

	if err := Send(Command{Cmd: "boom"}); err == nil || err.Error() != "处理失败" {
		t.Fatalf("应返回处理器错误，实际 %v", err)
	}
}

func TestSendWithoutServer(t *testing.T) {
	// 先占位再关闭，确保管道不存在。
	srv, err := Serve(func(Command) error { return nil })
	if err != nil {
		t.Fatalf("启动管道失败: %v", err)
	}
	srv.Close()

	if err := Ping(); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("无服务端时应返回 ErrNotRunning，实际 %v", err)
	}
}

func TestRestartServer(t *testing.T) {
	// 关闭后能立刻重新监听（验证 Close 不会让管道句柄/线程卡死）。
	for i := 0; i < 3; i++ {
		srv, err := Serve(func(Command) error { return nil })
		if err != nil {
			t.Fatalf("第 %d 次启动失败: %v", i, err)
		}
		if err := Ping(); err != nil {
			t.Fatalf("第 %d 次 ping 失败: %v", i, err)
		}
		srv.Close()
	}
}
