package app

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"golang.org/x/sys/windows"
)

// PortInUseError 表示启动前检查发现端口被占用——通常意味着已有另一个实例在运行
// （也包括"另一个会话/权限下启动的、看不见的实例"）。
//
// 带上占用者信息，是为了让"双击没反应/端口被占用"不再是一句无从下手的报错：
// 用户能立刻看到是哪个进程占着端口。
type PortInUseError struct {
	Role    string // 端口角色：传输 / 发现（KVM 用 Service.Start 自己的报错）
	Network string // "tcp" / "udp"
	Port    int
	Owner   string // 形如 "copywhere-v0.5.2-windows-amd64.exe (PID 1234)"，取不到时为空
	Err     error
}

func (e *PortInUseError) Error() string {
	msg := fmt.Sprintf("%s端口 %d/%s 被占用", e.Role, e.Port, e.Network)
	if e.Owner != "" {
		msg += "：占用者 " + e.Owner
	}
	msg += "（已有 copywhere 实例在运行？）"
	// 系统错误原文一并带出：占用可能是"其他会话的实例"，也可能是端口被
	// 排除段/安全软件占用，详情能帮用户（和 doctor）分清这两种情况。
	if e.Err != nil {
		msg += "；" + e.Err.Error()
	}
	return msg
}

func (e *PortInUseError) Unwrap() error { return e.Err }

// isAddrInUse 判断监听失败是否真的是"地址已被占用"。
//
// Windows 上 bind 失败的原因不止一种：端口落在 Hyper-V/WinNAT 的排除段、被
// 安全软件挂勾子等会返回 WSAEACCES（权限不足）。把这些一律说成"已有实例在运行"
// 会把用户引向错误方向，所以只有 WSAEADDRINUSE 才走端口占用这条归因。
// 注意用 windows.WSAEADDRINUSE（10048）：syscall.EADDRINUSE 在 Windows 上是
// 合成的 APPLICATION_ERROR 基准值，与真实 WSA 错误码对不上。
func isAddrInUse(err error) bool {
	return errors.Is(err, windows.WSAEADDRINUSE)
}

// PortOwner 返回占用本机某端口的进程描述（形如 "pwsh.exe (PID 1234)"），
// 取不到时返回空串。供 doctor 之类自检命令使用。
func PortOwner(port int, udp bool) string { return portOwner(port, udp) }

// portOwner 用 netstat 查出占用某端口的进程，返回 "程序名 (PID n)"。
// 只在端口冲突这条异常路径上调用，多花几十毫秒换实现简单、无 unsafe 风险。
func portOwner(port int, udp bool) string {
	proto := "tcp"
	if udp {
		proto = "udp"
	}
	out, err := exec.Command("netstat", "-ano", "-p", proto).Output()
	if err != nil {
		return ""
	}
	suffix := ":" + strconv.Itoa(port)
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		// 形如：TCP  0.0.0.0:47831  0.0.0.0:0  LISTENING  1234
		//       UDP  0.0.0.0:47830  *:*  1234
		if !strings.HasSuffix(fields[1], suffix) {
			continue
		}
		pid, err := strconv.Atoi(fields[len(fields)-1])
		if err != nil || pid <= 0 {
			continue
		}
		name := processImage(uint32(pid))
		if name == "" {
			name = "PID " + strconv.Itoa(pid)
		} else {
			name = fmt.Sprintf("%s (PID %d)", name, pid)
		}
		return name
	}
	return ""
}

// processImage 返回进程的可执行文件名（取不到返回空串）。
func processImage(pid uint32) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_PATH)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return ""
	}
	full := windows.UTF16ToString(buf[:size])
	for i := len(full) - 1; i >= 0; i-- {
		if full[i] == '\\' || full[i] == '/' {
			return full[i+1:]
		}
	}
	return full
}
