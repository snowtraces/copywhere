package transport

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"copywhere/internal/config"
)

// testToken 是测试约定的有效配对令牌.
const testToken = "test-pair-token"

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	cfg.ReceiveDir = t.TempDir()
	cfg.TransferPort = freePort(t)
	return cfg
}

func startServer(t *testing.T, cfg *config.Config) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_ = Server(ctx, cfg, Handlers{
			// 测试用鉴权：令牌 == testToken（生产中由 app 提供配对令牌校验）
			Authorize: func(hdr Header) bool { return hdr.Token == testToken },
		})
	}()
	// 等端口就绪
	for i := 0; i < 50; i++ {
		c, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", cfg.TransferPort), 100*time.Millisecond)
		if err == nil {
			c.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("server did not start")
}

func TestFileTransfer(t *testing.T) {
	cfg := testConfig(t)
	startServer(t, cfg)

	src := filepath.Join(t.TempDir(), "hello.txt")
	content := []byte("copywhere 传输测试 hello world")
	if err := os.WriteFile(src, content, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)

	send := func() (Response, error) {
		hdr := Header{V: 1, Token: testToken, Type: "file", Name: "hello.txt",
			Size: int64(len(content)), SHA256: hex.EncodeToString(sum[:]), Sender: "tester"}
		return Send("127.0.0.1", cfg.TransferPort, hdr, func(w io.Writer) error {
			_, err := w.Write(content)
			return err
		}, 10*time.Second)
	}

	resp, err := send()
	if err != nil {
		t.Fatalf("首次发送失败: %v", err)
	}
	if resp.Duplicate {
		t.Fatal("首次发送不应判定为重复")
	}
	got, err := os.ReadFile(resp.Path)
	if err != nil || string(got) != string(content) {
		t.Fatalf("接收内容不一致: %v", err)
	}

	// 同内容重发应判定为重复
	resp2, err := send()
	if err != nil {
		t.Fatalf("重发失败: %v", err)
	}
	if !resp2.Duplicate {
		t.Fatalf("同内容重发应判定为重复, resp=%+v", resp2)
	}
	if resp2.Path != resp.Path {
		t.Fatalf("重复内容路径应一致: %s vs %s", resp2.Path, resp.Path)
	}
}

func TestTextTransfer(t *testing.T) {
	cfg := testConfig(t)
	startServer(t, cfg)

	text := "你好，copywhere"
	sum := sha256.Sum256([]byte(text))
	hdr := Header{V: 1, Token: testToken, Type: "text", Name: "text",
		Size: int64(len(text)), SHA256: hex.EncodeToString(sum[:]), Sender: "tester"}
	resp, err := Send("127.0.0.1", cfg.TransferPort, hdr, func(w io.Writer) error {
		_, err := w.Write([]byte(text))
		return err
	}, 10*time.Second)
	if err != nil {
		t.Fatalf("文本发送失败: %v", err)
	}
	if !resp.OK {
		t.Fatalf("应答应为 OK: %+v", resp)
	}
}

func TestBadTokenRejected(t *testing.T) {
	cfg := testConfig(t)
	startServer(t, cfg)

	hdr := Header{V: 1, Token: "wrong-token", Type: "text", Name: "text",
		Size: 3, SHA256: "x", Sender: "tester"}
	resp, err := Send("127.0.0.1", cfg.TransferPort, hdr, func(w io.Writer) error {
		_, err := w.Write([]byte("abc"))
		return err
	}, 10*time.Second)
	if err == nil || resp.OK {
		t.Fatalf("错误 token 应被拒绝: resp=%+v err=%v", resp, err)
	}
}

func TestShaMismatchRejected(t *testing.T) {
	cfg := testConfig(t)
	startServer(t, cfg)

	hdr := Header{V: 1, Token: testToken, Type: "text", Name: "text",
		Size: 3, SHA256: hex.EncodeToString([]byte("bad")), Sender: "tester"}
	_, err := Send("127.0.0.1", cfg.TransferPort, hdr, func(w io.Writer) error {
		_, err := w.Write([]byte("abc"))
		return err
	}, 10*time.Second)
	if err == nil {
		t.Fatal("sha 不匹配应报错")
	}
}

// TestOversizeHeaderRejected 保证超长恶意头不会耗尽内存。
func TestOversizeHeaderRejected(t *testing.T) {
	cfg := testConfig(t)
	startServer(t, cfg)

	conn, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", cfg.TransferPort), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	big := make([]byte, 128*1024)
	for i := range big {
		big[i] = 'a'
	}
	conn.Write(big)
	conn.Write([]byte("\n"))
	line, err := bufio.NewReader(conn).ReadSlice('\n')
	if err != nil {
		t.Fatalf("未收到应答: %v", err)
	}
	var resp Response
	if json.Unmarshal(line, &resp) != nil || resp.OK {
		t.Fatalf("超长头应被拒绝: %+v", resp)
	}
}
