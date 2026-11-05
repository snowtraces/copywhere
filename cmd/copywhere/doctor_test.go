package main

import (
	"strings"
	"testing"
)

// 自检输出里的面板地址必须打码，否则贴到公开场合会泄露访问令牌。
func TestMaskToken(t *testing.T) {
	raw := "http://127.0.0.1:47890/?key=0123456789abcdef"
	got := maskToken(raw)
	if strings.Contains(got, "0123456789abcdef") || strings.Contains(got, "01234567") {
		t.Fatalf("令牌未被打码: %q", got)
	}
	if want := "http://127.0.0.1:47890/?key=****"; got != want {
		t.Fatalf("打码结果应为 %q，实际 %q", want, got)
	}
	// 没有令牌的地址原样返回
	if got := maskToken("http://127.0.0.1:47890/"); got != "http://127.0.0.1:47890/" {
		t.Fatalf("无令牌地址不应改动: %q", got)
	}
}

// gui.log 尾行里的面板地址（常出现在 doctor 输出中）同样要打码。
func TestMaskLogTokens(t *testing.T) {
	cases := []struct{ in, want string }{
		{
			"2026/11/05 12:00:00 控制面板: http://127.0.0.1:47890/?key=0123456789abcdef0123456789abcdef",
			"2026/11/05 12:00:00 控制面板: http://127.0.0.1:47890/?key=****",
		},
		{
			"http://127.0.0.1:1/?a=1&key=deadbeef&b=2",
			"http://127.0.0.1:1/?a=1&key=****&b=2",
		},
		{"复制成功，共 3 项", "复制成功，共 3 项"},
		// monkey= 这类普通文本不能被误伤
		{"节点 monkey=abc 的上报", "节点 monkey=abc 的上报"},
	}
	for _, c := range cases {
		if got := maskLogTokens(c.in); got != c.want {
			t.Fatalf("maskLogTokens(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

func TestURLPort(t *testing.T) {
	cases := map[string]int{
		"http://127.0.0.1:47890/?key=abc": 47890,
		"http://127.0.0.1:8080/":          8080,
		"http://127.0.0.1/":               0,
		"不是地址":                            0,
	}
	for raw, want := range cases {
		if got := urlPort(raw); got != want {
			t.Fatalf("urlPort(%q) = %d, 期望 %d", raw, got, want)
		}
	}
}
