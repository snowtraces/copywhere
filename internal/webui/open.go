package webui

import (
	"embed"
	"io"
	"os/exec"
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
