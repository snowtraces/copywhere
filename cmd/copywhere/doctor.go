package main

import (
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"copywhere/internal/app"
	"copywhere/internal/config"
	"copywhere/internal/shellmenu"
	"copywhere/internal/webui"
)

// cmdDoctor 打印一次只读自检报告。
//
// 针对"双击没反应 / 托盘没有图标 / 别的机器鼠标移不进来"这类只能到现场看的
// 问题：把进程身份、配置、单实例锁、面板地址与可达性、KVM/传输端口、右键菜单
// 指向、通知区域登记、gui.log 尾部一次性列出来，用户把输出贴回来即可定位。
// 全程不改任何状态（连互斥锁都是只探测、不持有）。
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
	rid := app.IntegrityRID()
	fmt.Printf("%s 完整性级别: %s (0x%X)\n", ok, app.IntegrityLabel(), rid)
	if app.LowIntegrity() {
		fmt.Printf("%s 进程处于低完整性级别：写不了用户目录、托盘图标会被丢弃、鼠标注入会被拒\n", bad)
		fmt.Printf("  低完整性级别可能来自安全软件、父进程权限或组织策略；请检查启动策略并按需允许 %s\n", exe)
	}
	elevated := windows.GetCurrentProcessToken().IsElevated()
	if elevated {
		fmt.Printf("%s 权限: 管理员（提权）——会自动打开面板的动作会被跳过，避免拉起提权浏览器\n", warn)
	} else {
		fmt.Printf("%s 权限: 普通用户\n", ok)
	}
	fmt.Printf("%s 会话: %d，进程 PID %d\n", ok, sessionID(), os.Getpid())

	section("配置")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Printf("%s 读取配置失败: %v\n", bad, err)
		cfg = nil
	} else {
		fmt.Printf("%s 路径: %s\n", ok, cfgPath)
		fmt.Printf("  节点名: %s\n", cfg.NodeName)
		fmt.Printf("  端口: 发现 %d/udp，传输 %d/tcp，KVM %d/tcp（开关 %v），面板 %d\n",
			cfg.DiscoveryPort, cfg.TransferPort, cfg.KVMPort, cfg.KVMOn(), cfg.WebPort)
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
	running := singletonTaken()
	if running {
		fmt.Printf("%s 已有实例持有单实例锁（gui 或 run 正在运行）\n", ok)
	} else {
		fmt.Printf("%s 没有实例在运行——此时面板、KVM 都不在，别的机器当然移不进来\n", bad)
	}

	section("面板")
	urlPath := webui.PanelURLPath(cfgPath)
	panelURL := ""
	if data, err := os.ReadFile(urlPath); err == nil {
		panelURL = strings.TrimSpace(string(data))
		fmt.Printf("%s 地址文件: %s\n", ok, urlPath)
		fmt.Printf("  地址: %s\n", maskToken(panelURL))
	} else {
		fmt.Printf("%s 地址文件缺失: %s（没有实例写入过，或实例启动时写失败）\n", bad, urlPath)
	}
	if running {
		port := cfgPort(cfg)
		if port == 0 && panelURL != "" {
			port = urlPort(panelURL)
		}
		if port > 0 {
			if err := dialable(port); err != nil {
				fmt.Printf("%s 面板端口 %d 连不上: %v\n", bad, port, err)
				if owner := app.PortOwner(port, false); owner != "" {
					fmt.Printf("  端口被 %s 占用\n", owner)
				}
			} else {
				fmt.Printf("%s 面板端口 %d 可连接\n", ok, port)
			}
		}
	}

	section("端口（KVM / 传输）")
	if cfg != nil {
		for _, it := range []struct {
			name string
			port int
		}{{"KVM", cfg.KVMPort}, {"传输", cfg.TransferPort}} {
			if it.port == 0 {
				continue
			}
			err := dialable(it.port)
			switch {
			case err == nil:
				fmt.Printf("%s %s端口 %d 正在监听（本机服务已起）\n", ok, it.name, it.port)
			case running:
				fmt.Printf("%s %s端口 %d 连不上，但单实例锁被占用——监听可能失败（端口冲突/被拦截）\n", bad, it.name, it.port)
				owner := app.PortOwner(it.port, it.name == "发现")
				if owner == "" {
					fmt.Printf("  没查出占用者；若占用者是别的会话/权限下的进程，这里可能看不到\n")
				} else {
					fmt.Printf("  占用者: %s\n", owner)
				}
			default:
				fmt.Printf("%s %s端口 %d 未监听（没有实例运行）\n", warn, it.name, it.port)
			}
		}
	}

	section("资源管理器右键菜单")
	cur, err := shellmenu.Current()
	switch {
	case err != nil:
		fmt.Printf("%s 读取注册表失败: %v\n", bad, err)
	case cur == "":
		fmt.Printf("%s 未注册\n", warn)
	case exe != "" && cur == shellmenu.Command(exe):
		fmt.Printf("%s 已注册并指向当前 exe\n", ok)
	default:
		fmt.Printf("%s 指向别的程序: %s\n", bad, cur)
		fmt.Printf("  本程序路径: %s\n", exe)
		fmt.Printf("  重启本程序会自动纠正\n")
	}

	section("通知区域（托盘图标）")
	printTrayState(exe, ok, bad, warn)

	section("日志")
	logPath := filepath.Join(filepath.Dir(cfgPath), guiLogName)
	if data, err := os.ReadFile(logPath); err == nil {
		lines := strings.Split(strings.TrimRight(string(data), "\r\n"), "\n")
		if len(lines) > 10 {
			lines = lines[len(lines)-10:]
		}
		fmt.Printf("%s %s（末尾 %d 行）\n", ok, logPath, len(lines))
		for _, l := range lines {
			// 日志里有带令牌的面板地址（gui 启动时会打印），逐行同样打码
			fmt.Printf("  %s\n", maskLogTokens(l))
		}
	} else {
		fmt.Printf("%s 没有 %s（gui 尚未成功启动过，或启动即被拦截）\n", bad, logPath)
	}

	fmt.Println()
	return nil
}

// printTrayState 报告托盘图标是否真的被创建，并解释图标到底在哪。
func printTrayState(exe string, ok, bad, warn string) {
	// 先看托盘窗口：这是"托盘代码有没有跑到"的硬证据。
	var ours []trayWindow
	for _, w := range findSystrayWindows() {
		if isCopywhereExe(w.exeName) {
			ours = append(ours, w)
		}
	}
	switch {
	case len(ours) > 0:
		for _, w := range ours {
			fmt.Printf("%s 托盘窗口已创建（PID %d，%s）——图标已交给系统\n", ok, w.pid, w.exeName)
		}
	case singletonTaken():
		fmt.Printf("%s 有实例在运行，但没有托盘窗口：该实例没走到创建托盘那一步\n", bad)
		fmt.Printf("  对照上面的日志与端口：面板/端口正常而托盘缺失，多半是托盘创建被拦\n")
	default:
		fmt.Printf("%s 没有实例在运行，也没有托盘窗口\n", warn)
	}

	// 再看 Windows 侧登记（Win11 NotifyIconSettings）：用来说明图标在不在折叠区。
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
		fmt.Printf("%s Windows 侧还没有该 exe 的图标记录（首次运行/未持久化），以托盘窗口为准\n", warn)
	}
}

// sameExePath 比较两个路径是否指向同一文件（容忍大小写与相对/绝对写法差异）。
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

// isCopywhereExe 判断进程名是不是本程序：发布包按 copywhere-*.exe 命名，
// 同时也认当前 exe 自己的文件名（改了名/开发构建也能被认出来）。
func isCopywhereExe(name string) bool {
	if name == "" {
		return false
	}
	lower := strings.ToLower(name)
	if strings.HasPrefix(lower, "copywhere") {
		return true
	}
	if exe, err := os.Executable(); err == nil {
		return lower == strings.ToLower(filepath.Base(exe))
	}
	return false
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

// maskToken 隐藏面板地址里的访问令牌，避免自检输出泄露凭据。
// 令牌整体打码：doctor 的输出经常被整段贴到公开场合反馈问题，保留前缀就是
// 白送出几位有效凭据。
func maskToken(u string) string {
	return maskLogTokens(u)
}

// maskLogTokens 把文本里出现的访问令牌（形如 ?key=<16 进制>）全部打码。
//
// gui.log 里"控制面板: http://127.0.0.1:PORT/?key=<32hex>"带的是仍然有效的
// 令牌，而 doctor 的用途恰恰是把输出发给别人看——原样打印等于把凭据交出去。
// 只认 ?key= / &key= 两种形式，避免误伤 monkey= 这类普通文本。
func maskLogTokens(line string) string {
	const marker = "key="
	var b strings.Builder
	rest := line
	for {
		i := strings.Index(rest, marker)
		if i < 0 {
			b.WriteString(rest)
			return b.String()
		}
		isURLParam := i == 0 || rest[i-1] == '?' || rest[i-1] == '&'
		b.WriteString(rest[:i+len(marker)])
		rest = rest[i+len(marker):]
		if !isURLParam {
			continue
		}
		n := 0
		for n < len(rest) && isTokenChar(rest[n]) {
			n++
		}
		if n == 0 {
			continue
		}
		b.WriteString("****")
		rest = rest[n:]
	}
}

// isTokenChar 判断字节是否可能属于访问令牌（crypto/rand 生成的 16 进制串）。
func isTokenChar(c byte) bool {
	switch {
	case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		return true
	}
	return false
}

// urlPort 从面板地址里取出端口（解析失败返回 0）。
func urlPort(raw string) int {
	u, err := url.Parse(raw)
	if err != nil {
		return 0
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		return 0
	}
	return port
}

// cfgPort 返回配置里的面板端口（配置为 nil 时返回 0）。
func cfgPort(cfg *config.Config) int {
	if cfg == nil {
		return 0
	}
	return cfg.WebPort
}
