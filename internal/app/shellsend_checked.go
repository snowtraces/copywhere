package app

import (
	"errors"
	"os"
	"strings"
)

// ShellSendChecked 与 ShellSend 相同，但先校验路径与可投递节点，
// 让 IPC 客户端（右键菜单 shell-send）能在失败时明确告诉用户原因。
func (a *App) ShellSendChecked(paths []string) error {
	valid, err := a.ValidateSendPaths(paths)
	if err != nil {
		return err
	}
	a.shellBatcher.Add(valid)
	return nil
}

// ValidateSendPaths 过滤不可访问的路径并确认至少有一个可投递的对端。
// 供 IPC 入口（右键发送、命令行委派）在真正建任务前给出明确原因。
func (a *App) ValidateSendPaths(paths []string) ([]string, error) {
	valid := make([]string, 0, len(paths))
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err != nil {
			continue
		}
		valid = append(valid, p)
	}
	if len(valid) == 0 {
		return nil, errors.New("没有可发送的文件（路径不存在或为空）")
	}
	if n, reason := a.sendablePeerSummary(); n == 0 {
		return nil, errors.New(reason)
	}
	return valid, nil
}

// ValidateSendText 校验文本内容并确认至少有一个可投递的对端。
func (a *App) ValidateSendText(text string) error {
	if strings.TrimSpace(text) == "" {
		return errors.New("文本不能为空")
	}
	if n, reason := a.sendablePeerSummary(); n == 0 {
		return errors.New(reason)
	}
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
