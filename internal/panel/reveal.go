package panel

import "os/exec"

// RevealInExplorer 在资源管理器中打开路径；highlight 为 true 时打开并选中该文件。
func RevealInExplorer(path string, highlight bool) error {
	if highlight {
		return exec.Command("explorer", "/select,", path).Start()
	}
	return exec.Command("explorer", path).Start()
}
