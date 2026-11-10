package app

import (
	"errors"
	"os"
)

// ShellSendChecked 与 ShellSend 相同，但先校验路径与可投递节点，
// 让 IPC 客户端（右键菜单 shell-send）能在失败时明确告诉用户原因。
func (a *App) ShellSendChecked(paths []string) error {
	valid := make([]string, 0, len(paths))
	for _, p := range paths {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err != nil {
			continue
		}
		valid = append(valid, p)
	}
	if len(valid) == 0 {
		return errors.New("没有可发送的文件（路径不存在或为空）")
	}
	if n, reason := a.sendablePeerSummary(); n == 0 {
		return errors.New(reason)
	}
	a.shellBatcher.Add(valid)
	return nil
}

// sendablePeerSummary 统计可直接投递的在线节点（已配对且未被鉴权拒绝），
// 为 0 时同时给出可展示的原因。
func (a *App) sendablePeerSummary() (int, string) {
	peers := a.Peers()
	if len(peers) == 0 {
		return 0, "未发现在线节点：请确认对端已运行 copywhere，且两台机器在同一网段"
	}
	n := 0
	for _, p := range peers {
		if a.PairedWith(p.ID) && !a.RejectedWith(p.ID) {
			n++
		}
	}
	if n == 0 {
		return 0, "在线节点均未配对（或鉴权被拒）：请先在窗口完成配对"
	}
	return n, ""
}
