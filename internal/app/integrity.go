package app

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// 完整性级别 RID（对应 S-1-16-*）：
//
//	0x0000 Untrusted、0x1000 Low、0x2000 Medium、0x3000 High、0x4000 System
const (
	integrityUntrusted = 0x0000
	integrityLow       = 0x1000
	integrityMedium    = 0x2000
	integrityHigh      = 0x3000
)

// IntegrityRID 返回当前进程的完整性级别 RID。
//
// 这只报告当前进程的令牌完整性级别，不能识别具体是哪款软件或策略设置了该级别。
// 低完整性进程在 Windows 上无权写用户目录与 HKCU、无法在通知区域注册图标、
// 也无法向普通权限的应用注入输入，表现就是"进程在跑，但没有托盘、文件不生成、鼠标移不进来"。
func IntegrityRID() uint32 {
	tok := windows.GetCurrentProcessToken()
	var n uint32
	if err := windows.GetTokenInformation(tok, windows.TokenIntegrityLevel, nil, 0, &n); err != nil && n == 0 {
		return integrityMedium // 读不到时按普通级别处理，不要误报
	}
	buf := make([]byte, n)
	if err := windows.GetTokenInformation(tok, windows.TokenIntegrityLevel, &buf[0], n, &n); err != nil {
		return integrityMedium
	}
	label := (*windows.SIDAndAttributes)(unsafe.Pointer(&buf[0]))
	if label.Sid == nil {
		return integrityMedium
	}
	count := label.Sid.SubAuthorityCount()
	if count == 0 {
		return integrityMedium
	}
	return label.Sid.SubAuthority(uint32(count - 1))
}

// IntegrityLabel 返回当前完整性级别的中文说明。
func IntegrityLabel() string {
	rid := IntegrityRID()
	switch {
	case rid < integrityLow:
		return "不可信（Untrusted）"
	case rid < integrityMedium:
		return "低完整性（受限运行）"
	case rid < integrityHigh:
		return "普通用户"
	case rid < 0x4000:
		return "管理员（提权）"
	default:
		return "系统"
	}
}

// LowIntegrity 报告当前进程是否被降级到低完整性级别（含更低的 Untrusted）。
//
// 命中时程序基本没有可用功能：写不了配置文件与日志、托盘图标会被资源管理器丢弃、
// 鼠标注入会被 UIPI 拒绝。此时应当明确告知用户去安全软件里加信任，而不是继续跑一个
// "看着在运行、其实全废"的实例去占着端口和单实例锁。
func LowIntegrity() bool { return IntegrityRID() < integrityMedium }

// SandboxedHint 说明低完整性级别的影响，并提示用户核对启动策略。
func SandboxedHint() string {
	return fmt.Sprintf(`copywhere 正在以「%s」运行。
该级别可能由安全软件隔离、父进程权限或组织策略导致；仅凭进程令牌无法判断具体来源。

低完整性级别下 Windows 不允许它：
  · 写用户目录（配置文件、日志、面板地址都写不出来）
  · 在通知区域注册图标（所以任务栏看不到图标）
  · 向普通权限的程序注入鼠标（所以别的电脑控制鼠标进不来）

请检查启动该程序的安全软件或组织策略，并按需允许下面这个文件以普通用户完整性运行：
%s

放行后重新双击，图标会出现在任务栏的通知区域（Windows 11 默认收在 ^ 折叠区）。`,
		IntegrityLabel(), ownExePath())
}

// ownExePath 返回当前可执行文件的完整路径（取不到时给一句占位）。
func ownExePath() string {
	p, err := os.Executable()
	if err != nil || p == "" {
		return "copywhere-v*.exe"
	}
	return p
}
