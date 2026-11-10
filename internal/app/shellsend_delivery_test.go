package app

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"copywhere/internal/config"
	"copywhere/internal/transport"
)

// deliveryCfg 构造一个用于本机环回测试的配置：独立目录（peers.json 不落到用户家目录）、
// 独立端口、关闭自动回贴（测试不碰真实剪贴板）。
func deliveryCfg(t *testing.T, dir string, transferPort int) *config.Config {
	t.Helper()
	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Path = filepath.Join(dir, "config.json")
	cfg.ReceiveDir = filepath.Join(dir, "files")
	cfg.TransferPort = transferPort
	cfg.DiscoveryPort = transferPort + 100 // 本测试不跑发现，只为不与他人抢端口
	cfg.AutoPaste = false
	cfg.TextSync = false
	return cfg
}

// waitPort 等待本机端口可连接（接收端 transport.Server 起来是异步的）。
func waitPort(t *testing.T, port int) {
	t.Helper()
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
			c.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("接收端端口 %d 未就绪", port)
}

// findFile 在目录树中查找指定文件名，返回其完整路径（未找到返回空串）。
func findFile(root, name string) string {
	var found string
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || found != "" {
			return nil
		}
		if d.Name() == name {
			found = p
		}
		return nil
	})
	return found
}

// TestShellSendDeliversToPeer 是右键菜单发送的端到端回归：
// ShellSend（右键入口）→ 合并窗口 → SendFiles → TCP 传输 → 对端落盘。
// 节点表与配对令牌直接注入，跳过 UDP 发现与人工配对（与本功能无关）。
func TestShellSendDeliversToPeer(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("仅 Windows（传输层依赖本机平台实现）")
	}
	recvCfg := deliveryCfg(t, t.TempDir(), 48961)
	sendCfg := deliveryCfg(t, t.TempDir(), 48962)

	recv, err := newAppWithID(recvCfg, "test-receiver")
	if err != nil {
		t.Fatal(err)
	}
	sender, err := newAppWithID(sendCfg, "test-sender")
	if err != nil {
		t.Fatal(err)
	}
	// 双向配对令牌：A 签发给 B 的（B 发送时携带）/ B 签发给 A 的
	const tokenAForB, tokenBForA = "token-a-for-b", "token-b-for-a"
	if err := recv.trust.Add("test-sender", "sender", tokenAForB, tokenBForA); err != nil {
		t.Fatal(err)
	}
	if err := sender.trust.Add("test-receiver", "receiver", tokenBForA, tokenAForB); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		if err := transport.Server(ctx, recvCfg, transport.Handlers{
			OnFile:    recv.onFileReceived,
			Authorize: recv.authorize,
		}); err != nil && ctx.Err() == nil {
			t.Logf("接收端传输服务退出: %v", err)
		}
	}()
	waitPort(t, recvCfg.TransferPort)

	// 注入对端节点（相当于发现层已把它记为本机节点）
	sender.store.Upsert("test-receiver", "receiver", "127.0.0.1", recvCfg.TransferPort, "")

	src := filepath.Join(t.TempDir(), "报告.txt")
	if err := os.WriteFile(src, []byte("right-click send"), 0o644); err != nil {
		t.Fatal(err)
	}
	sender.ShellSend([]string{src})

	deadline := time.Now().Add(10 * time.Second)
	var received string
	for time.Now().Before(deadline) {
		if received = findFile(recvCfg.ReceiveDir, "报告.txt"); received != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if received == "" {
		t.Fatal("对端没有收到右键发送的文件")
	}
	data, err := os.ReadFile(received)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "right-click send" {
		t.Fatalf("收到的内容不符: %q", data)
	}
}
