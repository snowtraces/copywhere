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
	"testing"
	"time"

	"copywhere/internal/config"
)

// shaHex 是测试内常用的 sha256 十六进制串。
func shaHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// startServerH 以自定义 Handlers 启动测试服务（覆盖 OnFile 之外的回调场景），
// 与 transport_test.go 的 startServer 并列，用于 rich/text 等消息类型。
func startServerH(t *testing.T, cfg *config.Config, h Handlers) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = Server(ctx, cfg, h) }()
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

// TestRichTransfer 验证 rich 消息端到端：头部类型、JSON 负载（HTML/RTF 原样）、
// 校验与回调内容一致。
func TestRichTransfer(t *testing.T) {
	cfg := testConfig(t)
	got := make(chan RichPayload, 1)
	startServerH(t, cfg, Handlers{
		Authorize: func(hdr Header) bool { return hdr.Token == testToken },
		OnRich: func(sender string, p RichPayload) {
			if sender != "tester" {
				t.Errorf("sender = %q", sender)
			}
			got <- p
		},
	})

	payload, _ := json.Marshal(RichPayload{
		Text: "加粗文字",
		HTML: []byte("<h1>hi</h1>"),
		RTF:  []byte(`{\rtf1 bold}`),
	})
	hdr := Header{V: 1, Token: testToken, Type: "rich", Name: "rich",
		Size: int64(len(payload)), SHA256: shaHex(payload), Sender: "tester"}
	if _, err := Send("127.0.0.1", cfg.TransferPort, hdr, func(w io.Writer) error {
		_, err := w.Write(payload)
		return err
	}, 10*time.Second); err != nil {
		t.Fatalf("rich 发送失败: %v", err)
	}
	select {
	case p := <-got:
		if p.Text != "加粗文字" || string(p.HTML) != "<h1>hi</h1>" || string(p.RTF) != `{\rtf1 bold}` {
			t.Fatalf("回调内容与发送不一致: %+v", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("未收到 OnRich 回调")
	}
}

// TestRichWithoutHandlerFallsBackToText 服务端未接 OnRich 时，富文本降级
// 为纯文本回调（对端仍为旧版本时 app 层根本不会发 rich，双保险）。
func TestRichWithoutHandlerFallsBackToText(t *testing.T) {
	cfg := testConfig(t)
	got := make(chan string, 1)
	startServerH(t, cfg, Handlers{
		Authorize: func(hdr Header) bool { return hdr.Token == testToken },
		OnText:    func(sender, text string) { got <- text },
	})
	payload, _ := json.Marshal(RichPayload{Text: "纯文本兜底", HTML: []byte("<b>x</b>")})
	hdr := Header{V: 1, Token: testToken, Type: "rich", Name: "rich",
		Size: int64(len(payload)), SHA256: shaHex(payload), Sender: "tester"}
	if _, err := Send("127.0.0.1", cfg.TransferPort, hdr, func(w io.Writer) error {
		_, err := w.Write(payload)
		return err
	}, 10*time.Second); err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	select {
	case s := <-got:
		if s != "纯文本兜底" {
			t.Fatalf("应回落到纯文本，实际 %q", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("未收到 OnText 兜底回调")
	}
}

// TestRichRejected 覆盖 recvRich 的错误路径：size 超限/负载截断（半关闭
// 触发提前 EOF）/sha 不符/合法 sha 但畸形 JSON——最后一条是校验链之后
// 仅存的解析面。全部必须收到错误应答，且 OnRich/OnText 均不被调用。
func TestRichRejected(t *testing.T) {
	cases := []struct {
		name  string
		size  int64 // 头部声明值
		shaOf func(body []byte) string
		body  string
	}{
		{"size超限", MaxRichBytes + 1, shaHex, `{"text":"x"}`},
		{"截断EOF", 100, shaHex, `{"text":"x"}`}, // 声明 100 实发 12，半关闭后 EOF
		{"sha不符", 14, func([]byte) string { return "deadbeef" }, `{"text":"hello"}`},
		{"畸形JSON", 4, shaHex, `null`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t)
			got := make(chan any, 1)
			startServerH(t, cfg, Handlers{
				Authorize: func(hdr Header) bool { return hdr.Token == testToken },
				OnRich:    func(string, RichPayload) { got <- "rich" },
				OnText:    func(string, string) { got <- "text" },
			})
			conn, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", cfg.TransferPort), 3*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(8 * time.Second))
			h, _ := json.Marshal(Header{V: 1, Token: testToken, Type: "rich", Name: "rich",
				Size: tc.size, SHA256: tc.shaOf([]byte(tc.body)), Sender: "tester"})
			if _, err := conn.Write(append(h, '\n')); err != nil {
				t.Fatal(err)
			}
			if _, err := conn.Write([]byte(tc.body)); err != nil {
				t.Fatal(err)
			}
			if tc.name == "截断EOF" {
				if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
					t.Fatal(err) // 半关闭：让服务端读到 EOF 判定不完整
				}
			}
			line, err := bufio.NewReader(conn).ReadBytes('\n')
			if err != nil {
				t.Fatalf("未收到应答: %v", err)
			}
			var resp Response
			if json.Unmarshal(line, &resp) != nil || resp.OK {
				t.Fatalf("应被拒绝: %s", line)
			}
			select {
			case v := <-got:
				t.Fatalf("被拒负载不应触发回调: %v", v)
			default:
			}
		})
	}
}
