//go:build windows

package clip

import (
	"bytes"
	"testing"
	"time"
)

// waitRich 等待刚写入的富文本真正可见。
//
// Windows 上 SetClipboardData/CloseClipboard 之后，新内容对本进程的后续读取
// 可能有一两毫秒不可见（本机开着别的剪贴板监听者时更容易遇到）：立即 ReadRich
// 会拿到空文本/缺格式，等一下再读就齐全了。这是剪贴板发布时序，不是读写逻辑
// 缺陷，因此这里只对"还没生效"重试；超时后仍把最后一次读到的内容交给调用方
// 逐项断言——真正的嵌套开关剪贴板回归（文本在、富格式丢）依然会让测试失败。
func waitRich(t *testing.T, want Rich) Rich {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var last Rich
	var lastErr error
	for {
		got, err := ReadRich()
		last, lastErr = got, err
		if err == nil &&
			got.Text == want.Text &&
			bytes.Equal(got.HTML, want.HTML) &&
			bytes.Equal(got.RTF, want.RTF) {
			return got
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				t.Fatalf("ReadRich: %v（等待 2s 仍不可见）", lastErr)
			}
			return last
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestRichRoundTrip 是 B2 富文本读取路径的回归锁：SetRich 写入
// 文本+HTML+RTF 后 ReadRich 必须原样读回三种格式。
//
// 曾经的实际缺陷（审查发现）：readRichLocked 嵌套调用会自行
// OpenClipboard/CloseClipboard 的 readTextLocked，内层 close 把剪贴板
// 提前还给系统，此后 HTML/RTF 读取静默失败——若只测纯文本永远发现不了。
func TestRichRoundTrip(t *testing.T) {
	prev, perr := ReadRich() // 礼貌起见结束后尽量恢复原内容
	want := Rich{
		Text: "加粗文字 hello",
		HTML: []byte("Version:0.9\r\nStartFragment: G0000007\r\n<h1>x</h1>"),
		RTF:  []byte(`{\rtf1\ansi bold}`),
	}
	if err := SetRich(want); err != nil {
		t.Skipf("剪贴板不可用（无交互会话或被占用）: %v", err)
	}
	t.Cleanup(func() {
		if perr == nil && prev.Text != "" {
			_ = SetRich(prev)
		}
	})

	got := waitRich(t, want)
	if got.Text != want.Text {
		t.Errorf("Text = %q, want %q", got.Text, want.Text)
	}
	if string(got.HTML) != string(want.HTML) {
		t.Errorf("HTML 丢失或损坏（嵌套开关剪贴板回归？）: %q", got.HTML)
	}
	if string(got.RTF) != string(want.RTF) {
		t.Errorf("RTF 丢失或损坏: %q", got.RTF)
	}
	if !got.HasFormats() {
		t.Error("HasFormats() = false，应为 true")
	}
}

// TestRichMagicReject 验证写侧轻校验：魔数不符（非 CF_HTML 的
// "Version:" 头、非 "{\rtf" 开头的 RTF）的可疑内容在写回本机剪贴板
// 之前就被丢弃，只保留纯文本——不把畸形数据递给 Word/浏览器的解析器。
func TestRichMagicReject(t *testing.T) {
	r := Rich{
		Text: "plain",
		HTML: []byte("<script>alert(1)</script>"), // 无 Version: 头
		RTF:  []byte("not rtf at all"),
	}
	if err := SetRich(r); err != nil {
		t.Skipf("剪贴板不可用: %v", err)
	}
	t.Cleanup(func() {
		if prev, err := ReadRich(); err == nil && prev.Text == "plain" {
			_ = SetText("roundtrip-cleanup") // 清掉本测试写的内容
		}
	})
	got, err := ReadRich()
	if err != nil {
		t.Fatalf("ReadRich: %v", err)
	}
	if got.Text != "plain" {
		t.Errorf("Text = %q", got.Text)
	}
	if len(got.HTML) != 0 || len(got.RTF) != 0 {
		t.Errorf("畸形格式应被写侧丢弃，got html=%q rtf=%q", got.HTML, got.RTF)
	}
}
