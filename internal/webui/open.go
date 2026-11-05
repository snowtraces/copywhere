package webui

import (
	"embed"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

//go:embed static/index.html
var staticFS embed.FS

// indexHTML 是内嵌面板页面。
var indexHTML = func() []byte {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		return []byte("面板资源缺失: " + err.Error())
	}
	return b
}()

// copyAndClose 把 src 拷入 dst 并关闭 dst，返回写入字节数。
func copyAndClose(src io.Reader, dst io.WriteCloser) (int64, error) {
	defer dst.Close()
	return io.Copy(dst, src)
}

// RevealInExplorer 在资源管理器中打开目录；highlight 为 true 时打开并选中该文件。
func RevealInExplorer(path string, highlight bool) error {
	if highlight {
		return exec.Command("explorer", "/select,", path).Start()
	}
	return exec.Command("explorer", path).Start()
}

// OpenBrowser 用系统默认浏览器打开 URL。
func OpenBrowser(url string) error {
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}

// ErrBrowserElevated 表示当前环境会把 URL 交给"管理员权限的浏览器实例"，
// 已主动跳过自动打开（而不是把浏览器搞崩）。
var ErrBrowserElevated = errors.New("当前环境会打开管理员权限的浏览器实例，已跳过自动打开")

// browserExeNames 是检查提权实例时关注的浏览器进程名（不含 .exe，小写）。
var browserExeNames = []string{
	"msedge", "chrome", "firefox", "brave", "opera", "vivaldi", "chromium",
	"iexplore", "360se", "360chrome", "qqbrowser", "sogouexplorer", "arc", "zen",
}

// browserInstance 是一个正在运行的浏览器进程。
type browserInstance struct {
	pid      uint32
	name     string
	elevated bool
}

// runningBrowsers 列出正在运行的浏览器进程及其提权状态。
func runningBrowsers() []browserInstance {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(snap)
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	var out []browserInstance
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		name := strings.ToLower(windows.UTF16ToString(e.ExeFile[:]))
		name = strings.TrimSuffix(name, ".exe")
		if !isBrowserExe(name) {
			continue
		}
		out = append(out, browserInstance{pid: e.ProcessID, name: name, elevated: processElevated(e.ProcessID)})
	}
	return out
}

// isBrowserExe 判断进程名是否为已知浏览器。
func isBrowserExe(name string) bool {
	for _, b := range browserExeNames {
		if name == b {
			return true
		}
	}
	return false
}

// processElevated 报告某进程是否以管理员权限运行（无法查询时按"否"处理）。
func processElevated(pid uint32) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var tok windows.Token
	if err := windows.OpenProcessToken(h, windows.TOKEN_QUERY, &tok); err != nil {
		return false
	}
	defer tok.Close()
	return tok.IsElevated()
}

// selfElevated 报告当前进程是否以管理员权限运行。
func selfElevated() bool {
	return windows.GetCurrentProcessToken().IsElevated()
}

// BrowserElevationIssue 说明为什么此时不该自动打开浏览器；空字符串表示可以打开。
//
// 提权进程（例如从管理员权限的资源管理器双击启动）把 URL 交给浏览器时，会拉起
// 或复用一个管理员权限的浏览器实例；此后普通权限启动浏览器，系统就会弹
// 「现有实例正在以提升的权限运行，是否要用普通权限重启现有实例？」并可能把
// 浏览器卡死。因此这种情况下只把面板地址写进日志与 panel_url.txt，不自动打开。
func BrowserElevationIssue() string {
	if selfElevated() {
		return "copywhere 以管理员权限运行"
	}
	for _, b := range runningBrowsers() {
		if b.elevated {
			return fmt.Sprintf("%s（PID %d）以管理员权限运行", b.name, b.pid)
		}
	}
	return ""
}

// OpenBrowserChecked 在确知不会拉起提权浏览器时打开面板；否则返回
// ErrBrowserElevated 并附上原因，调用方只需记日志（面板地址仍可手动访问）。
func OpenBrowserChecked(url string) error {
	if reason := BrowserElevationIssue(); reason != "" {
		return fmt.Errorf("%w：%s", ErrBrowserElevated, reason)
	}
	return OpenBrowser(url)
}
