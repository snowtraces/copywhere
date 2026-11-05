// Package shellmenu 在 Windows 资源管理器右键菜单中注册「用 copywhere 发送」，
// 菜单命令指向当前 exe 的 shell-send 子命令。
//
// 实现方式是经典的静态 shell 动词（HKCU 下 Software\Classes\<类型>\shell\<动词>
// +\command 的默认值），而不是 IContextMenu COM 扩展：
//
//   - 不需要注册 CLSID/DLL、不做进程内注入，杀软与稳定性风险最低；
//   - 只写当前用户分支，无需管理员权限，卸载即彻底清除；
//   - 多选时资源管理器默认按"每项调用一次"分发（MultiSelectModel 的 Document
//     语义），每次调用是独立短进程——主程序用很窄的合并窗口把同一批选择拼成
//     一次传输（见 app.pathBatcher），对端因此只收到一个包而不是 N 条消息。
package shellmenu

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows/registry"
)

const (
	// classesRoot 是用户级文件关联根（HKCU，无需管理员权限）。
	classesRoot = `Software\Classes`
	// verbName 是右键菜单项在注册表中的键名。
	verbName = "copywhere"
	// MenuText 是右键菜单上显示的文字。
	MenuText = "用 copywhere 发送"
)

// targets 是挂载菜单的作用对象：全部文件类型与全部文件夹。
// （`*` 只覆盖文件，文件夹须单独用 Directory。）
var targets = []string{"*", "Directory"}

// classesRootPath 是注册表根路径；测试替换为临时分支，避免污染真实菜单。
var classesRootPath = classesRoot

// verbKeyPath 返回某个作用对象下菜单项键的完整路径。
func verbKeyPath(target string) string {
	return classesRootPath + `\` + target + `\shell\` + verbName
}

// Command 返回菜单项调用的完整命令行；%1 由资源管理器替换为被右键的路径。
func Command(exe string) string {
	return `"` + exe + `" shell-send "%1"`
}

// Install 把菜单项注册到当前用户（幂等，重复调用只覆盖为当前 exe）。
// exe 必须是绝对路径：菜单可能在任意工作目录下被点击。
func Install(exe string) error {
	if exe == "" {
		return errors.New("缺少 copywhere.exe 路径")
	}
	for _, target := range targets {
		keyPath := verbKeyPath(target)
		if err := installOne(keyPath, exe); err != nil {
			return fmt.Errorf("注册 %q 右键菜单失败: %w", target, err)
		}
	}
	return nil
}

// installOne 写入单个菜单项：显示文字、图标、禁止成为默认动作，以及 command 子键。
func installOne(keyPath, exe string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, keyPath, registry.SET_VALUE|registry.CREATE_SUB_KEY)
	if err != nil {
		return err
	}
	defer k.Close()
	if err := k.SetStringValue("MUIVerb", MenuText); err != nil {
		return err
	}
	// 图标直接取 exe 内嵌资源（rsrc_windows_amd64.syso）
	if err := k.SetStringValue("Icon", exe); err != nil {
		return err
	}
	// 空值即"声明存在"：避免资源管理器把它当成双击的默认动作
	if err := k.SetStringValue("NeverDefault", ""); err != nil {
		return err
	}
	cmdKey, _, err := registry.CreateKey(registry.CURRENT_USER, keyPath+`\command`, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer cmdKey.Close()
	return cmdKey.SetStringValue("", Command(exe))
}

// Uninstall 移除本包注册的全部菜单项；未注册时视为成功（幂等）。
func Uninstall() error {
	for _, target := range targets {
		keyPath := verbKeyPath(target)
		// 先删子键：DeleteKey 对仍有子键的键会失败
		if err := registry.DeleteKey(registry.CURRENT_USER, keyPath+`\command`); err != nil && !isNotExist(err) {
			return fmt.Errorf("移除 %q 右键菜单命令失败: %w", target, err)
		}
		if err := registry.DeleteKey(registry.CURRENT_USER, keyPath); err != nil && !isNotExist(err) {
			return fmt.Errorf("移除 %q 右键菜单失败: %w", target, err)
		}
	}
	return nil
}

// Installed 报告菜单项是否已注册（全部作用对象都在才算）。
// exe 非空时还要求菜单指向该 exe——程序被移动或升级后据此自动纠正。
func Installed(exe string) bool {
	for _, target := range targets {
		k, err := registry.OpenKey(registry.CURRENT_USER, verbKeyPath(target)+`\command`, registry.QUERY_VALUE)
		if err != nil {
			return false
		}
		cmd, _, err := k.GetStringValue("")
		k.Close()
		if err != nil || cmd == "" {
			return false
		}
		if exe != "" && cmd != Command(exe) {
			return false
		}
	}
	return true
}

// Current 返回菜单项当前注册的命令行（未注册时返回空串）。
//
// 供 `copywhere doctor` 之类的自检使用：升级/移动 exe 后菜单可能仍指向旧路径。
func Current() (string, error) {
	for _, target := range targets {
		k, err := registry.OpenKey(registry.CURRENT_USER, verbKeyPath(target)+`\command`, registry.QUERY_VALUE)
		if err != nil {
			if isNotExist(err) {
				return "", nil
			}
			return "", err
		}
		cmd, _, err := k.GetStringValue("")
		k.Close()
		if err != nil {
			if errors.Is(err, registry.ErrNotExist) {
				return "", nil
			}
			return "", err
		}
		return cmd, nil
	}
	return "", nil
}

// isNotExist 判断注册表键不存在（卸载时的正常分支）。
func isNotExist(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, registry.ErrNotExist)
}
