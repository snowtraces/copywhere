package webui

import "testing"

func TestIsBrowserExe(t *testing.T) {
	for _, name := range []string{"msedge", "chrome", "firefox", "360se"} {
		if !isBrowserExe(name) {
			t.Fatalf("%q 应识别为浏览器", name)
		}
	}
	for _, name := range []string{"notepad", "explorer", "copywhere", ""} {
		if isBrowserExe(name) {
			t.Fatalf("%q 不应识别为浏览器", name)
		}
	}
}

// 提权判断必须给出确定结论（要么可打开、要么给出具体原因），不能既说能又说不能。
func TestBrowserElevationIssueConsistent(t *testing.T) {
	reason := BrowserElevationIssue()
	if reason != "" {
		if err := OpenBrowserChecked("http://127.0.0.1:1/?key=x"); err == nil {
			t.Fatalf("已判定为提权环境（%s），OpenBrowserChecked 却返回成功", reason)
		}
	}
}
