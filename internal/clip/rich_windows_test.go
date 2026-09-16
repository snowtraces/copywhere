//go:build windows

package clip

import (
	"testing"
)

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

	got, err := ReadRich()
	if err != nil {
		t.Fatalf("ReadRich: %v", err)
	}
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
