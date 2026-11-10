package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"copywhere/internal/app"
	"copywhere/internal/config"
	"copywhere/internal/ipc"
	"copywhere/internal/shellmenu"
)

// cmdDoctor 打印一次只读自检报告。
//
// 针对"双击没反应 / 托盘没有图标 / 别的机器鼠标移不进来"这类只能到现场看的
// 问题：把进程身份、配置、单实例与 IPC、WebView2 运行时、KVM/传输端口、
// 右键菜单指向、通知区域登记、gui.log 尾部一次性列出来，用户把输出贴回来
// 即可定位。全程不改任何状态（互斥锁只探测、不持有）。
func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	configPath := fs.String("config", "", "配置文件路径")
	fs.Parse(args)

	cfgPath := *configPath
	if cfgPath == "" {
		cfgPath = config.DefaultPath()
	}
	exe, _ := os.Executable()
	section := func(name string) { fmt.Printf("\n=== %s ===\n", name) }
	ok, bad, warn := "✔", "✘", "!"

	section("程序")
	fmt.Printf("%s 版本: %s (%s/%s)\n", ok, app.Version, runtime.GOOS, runtime.GOARCH)
	fmt.Printf("%s 路径: %s\n", ok, exe)
	fmt.Printf("%s 完整性级别: %s (0x%X)\n", ok, app.IntegrityLabel(), app.IntegrityRID())
	if app.LowIntegrity() {
		fmt.Printf("%s 进程处于低完整性级别：写不了用户目录、托盘图标会被丢弃、输入注入会被拒\n", bad)
		fmt.Printf("  请检查启动策略并按需允许 %s\n", exe)
	}
	if windows.GetCurrentProcessToken().IsElevated() {
		fmt.Printf("%s 权限: 管理员（提权）\n", warn)
	} else {
		fmt.Printf("%s 权限: 普通用户\n", ok)
	}
	fmt.Printf("%s 会话: %d，进程 PID %d\n", ok, sessionID(), os.Getpid())

	section("配置")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Printf("%s 读取配置失败: %v\n", bad, err)
	} else {
		fmt.Printf("%s 路径: %s\n", ok, cfgPath)
		fmt.Printf("  节点名: %s\n", cfg.NodeName)
		fmt.Printf("  端口: 发现 %d/udp，传输 %d/tcp，KVM %d/tcp（开关 %v）\n",
			cfg.DiscoveryPort, cfg.TransferPort, cfg.KVMPort, cfg.KVMOn())
		dz := cfg.KVMEdgeDeadZone()
		dzText := fmt.Sprintf("%d 逻辑像素", dz)
		if dz < 0 {
			dzText = "48 逻辑像素（默认）"
		} else if dz == 0 {
			dzText = "已关闭"
		}
		fmt.Printf("  KVM 左邻=%q 右邻=%q 边缘死区=%s\n", cfg.KVMLeft, cfg.KVMRight, dzText)
		fmt.Printf("  右键菜单开关: %v\n", cfg.ShellMenuOn())
		fmt.Printf("  接收目录: %s\n", cfg.ReceiveDir)
	}

	section("运行状态")
	ipcErr := ipc.Ping()
	windowRunning := ipcErr == nil
	running := singletonTaken() || windowRunning
	if running {
		fmt.Printf("%s 已有实例持有单实例锁（桌面窗口或 run 正在运行）\n", ok)
	} else {
		fmt.Printf("%s 没有实例在运行——此时窗口、KVM 都不在，别的机器当然移不进来\n", bad)
	}
	if windowRunning {
		fmt.Printf("%s 本机 IPC 可用（右键发送/二次唤起正常）\n", ok)
	} else if running {
		fmt.Printf("%s 有实例但 IPC 连不上：可能是 run（无窗口）模式，或实例仍在启动\n", warn)
	} else {
		fmt.Printf("%s 本机 IPC 不可用（没有实例）\n", warn)
	}

	section("桌面运行时（WebView2）")
	if ver := webView2Version(); ver != "" {
		fmt.Printf("%s WebView2 运行时已安装: %s\n", ok, ver)
	} else {
		fmt.Printf("%s 未检测到 WebView2 运行时：窗口无法创建，请安装 Microsoft Edge WebView2 Runtime\n", bad)
	}

	section("端口（发现 / 传输 / KVM）")
	if cfg != nil {
		checkUDP := func(name string, port int) {
			if port <= 0 {
				return
			}
			c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: port})
			if err == nil {
				c.Close()
				if windowRunning {
					fmt.Printf("%s %s端口 %d/udp 未监听（实例在运行，但发现服务可能启动失败）\n", bad, name, port)
				} else {
					fmt.Printf("%s %s端口 %d/udp 可用\n", ok, name, port)
				}
				return
			}
			if windowRunning {
				fmt.Printf("%s %s端口 %d/udp 正在监听\n", ok, name, port)
			} else if owner := app.PortOwner(port, true); owner != "" {
				fmt.Printf("%s %s端口 %d/udp 被占用: %s\n", warn, name, port, owner)
			} else {
				fmt.Printf("%s %s端口 %d/udp 不可用: %v\n", warn, name, port, err)
			}
		}
		checkTCP := func(name string, port int) {
			if port <= 0 {
				return
			}
			if err := dialable(port); err == nil {
				fmt.Printf("%s %s端口 %d/tcp 正在监听（本机服务已起）\n", ok, name, port)
				return
			}
			if windowRunning {
				fmt.Printf("%s %s端口 %d/tcp 连不上，但实例在运行（监听可能失败/被拦截）\n", bad, name, port)
			} else {
				fmt.Printf("%s %s端口 %d/tcp 未监听（没有实例运行）\n", warn, name, port)
			}
			if owner := app.PortOwner(port, false); owner != "" {
				fmt.Printf("  占用者: %s\n", owner)
			}
		}
		checkUDP("发现", cfg.DiscoveryPort)
		checkTCP("传输", cfg.TransferPort)
		if cfg.KVMOn() {
			checkTCP("KVM", cfg.KVMPort)
		} else {
			fmt.Printf("%s KVM 已按配置停用（kvm_enabled=false）\n", ok)
		}
	}

	section("资源管理器右键菜单")
	cur, err := shellmenu.Current()
	switch {
	case err != nil:
		fmt.Printf("%s 读取注册表失败: %v\n", bad, err)
	case cur == "":
		fmt.Printf("%s 未注册（桌面窗口启动时会自动注册）\n", warn)
	case exe != "" && cur == shellmenu.Command(exe):
		fmt.Printf("%s 已注册并指向当前 exe\n", ok)
	default:
		fmt.Printf("%s 指向别的程序: %s\n", bad, cur)
		fmt.Printf("  本程序路径: %s（重启桌面窗口会自动纠正）\n", exe)
	}

	section("通知区域（托盘图标）")
	printNotifyIconState(exe, ok, bad, warn)

	section("日志")
	logPath := filepath.Join(filepath.Dir(cfgPath), guiLogName)
	if _, statErr := os.Stat(logPath); statErr != nil {
		if alt := filepath.Join(filepath.Dir(config.DefaultPath()), guiLogName); fileExists(alt) {
			logPath = alt
		}
	}
	if data, err := os.ReadFile(logPath); err == nil {
		lines := strings.Split(strings.TrimRight(string(data), "\r\n"), "\n")
		if len(lines) > 10 {
			lines = lines[len(lines)-10:]
		}
		fmt.Printf("%s %s（末尾 %d 行）\n", ok, logPath, len(lines))
		for _, l := range lines {
			fmt.Printf("  %s\n", l)
		}
	} else {
		fmt.Printf("%s 没有 %s（桌面窗口尚未成功启动过，或启动即被拦截）\n", bad, logPath)
	}

	fmt.Println()
	return nil
}

// printNotifyIconState 报告 Windows 侧是否为当前 exe 记录了通知区域图标，
// 以及是否被固定在任务栏（Win11 的 NotifyIconSettings）。
func printNotifyIconState(exe string, ok, bad, warn string) {
	const key = `Control Panel\NotifyIconSettings`
	root, err := registry.OpenKey(registry.CURRENT_USER, key, registry.READ)
	if err != nil {
		fmt.Printf("%s 读不到 %s: %v（Windows 10 及更早没有该键，属正常）\n", warn, key, err)
		return
	}
	defer root.Close()
	names, err := root.ReadSubKeyNames(-1)
	if err != nil {
		fmt.Printf("%s 枚举失败: %v\n", warn, err)
		return
	}
	found := 0
	for _, name := range names {
		k, err := registry.OpenKey(registry.CURRENT_USER, key+`\`+name, registry.READ)
		if err != nil {
			continue
		}
		path, _, _ := k.GetStringValue("ExecutablePath")
		prom, _, _ := k.GetIntegerValue("IsPromoted")
		k.Close()
		if path == "" || !sameExePath(path, exe) {
			continue
		}
		found++
		if prom != 0 {
			fmt.Printf("%s 图标已固定显示（IsPromoted=1）\n", ok)
		} else {
			fmt.Printf("%s 图标未固定：默认收在任务栏 ^ 折叠区，拖到任务栏即可常驻\n", warn)
		}
	}
	if found == 0 {
		fmt.Printf("%s Windows 侧还没有该 exe 的图标记录（首次运行/未持久化）\n", warn)
	}
}

// sameExePath 比较两个路径是否指向同一文件（容忍大小写与相对/绝对写法差异）。
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func sameExePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if strings.EqualFold(a, b) {
		return true
	}
	na, err1 := filepath.Abs(a)
	nb, err2 := filepath.Abs(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return strings.EqualFold(na, nb)
}

// webView2Version 从注册表读取 Evergreen WebView2 运行时版本；未安装返回空串。
func webView2Version() string {
	const guid = `{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}`
	for _, path := range []string{
		`SOFTWARE\WOW6432Node\Microsoft\EdgeUpdate\Clients\` + guid,
		`SOFTWARE\Microsoft\EdgeUpdate\Clients\` + guid,
	} {
		k, err := registry.OpenKey(registry.LOCAL_MACHINE, path, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		pv, _, err := k.GetStringValue("pv")
		k.Close()
		if err == nil && pv != "" {
			return pv
		}
	}
	return ""
}

// singletonTaken 探测单实例锁是否已被别的进程持有（探测后立即释放，不影响对方）。
func singletonTaken() bool {
	name, err := windows.UTF16PtrFromString(`Local\copywhere-singleton`)
	if err != nil {
		return false
	}
	h, err := windows.CreateMutex(nil, false, name)
	if err != nil {
		return true // ERROR_ALREADY_EXISTS：已有实例
	}
	windows.CloseHandle(h)
	return false
}

// sessionID 返回当前进程所在会话 ID。
func sessionID() uint32 {
	var sid uint32
	if err := windows.ProcessIdToSessionId(uint32(os.Getpid()), &sid); err != nil {
		return 0
	}
	return sid
}

// dialable 测试本机回环端口能否建立连接。
func dialable(port int) error {
	c, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", port), 800*time.Millisecond)
	if err != nil {
		return err
	}
	return c.Close()
}
