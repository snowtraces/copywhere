// copywhere — 局域网剪贴板/文件自动同步工具（Windows）。
//
// 在一台机器上 Ctrl+C 复制文件（小于阈值，默认 10MB）或文本，
// 局域网内所有在线节点的剪贴板都会出现同样的内容，直接 Ctrl+V 即可。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/term"

	"copywhere/internal/app"
	"copywhere/internal/clip"
	"copywhere/internal/config"
	"copywhere/internal/shellmenu"
	"copywhere/internal/webui"
)

func usage() {
	fmt.Print(`copywhere — 局域网剪贴板/文件自动同步工具

用法:
  copywhere init                            生成默认配置
  copywhere run [-config 路径] [-plain]     启动服务（节点发现 + 传输 + 剪贴板监控）
  copywhere gui [-config 路径]              启动服务 + 系统托盘 + 浏览器控制面板
  copywhere nodes [-config 路径] [-wait 4s] 扫描并列出局域网在线节点
  copywhere send [-config 路径] [-text 内容] [-wait 4s] 文件/目录...
                                            立即发送到所有在线节点（不受大小阈值限制）
  copywhere shell [-install|-uninstall|-status]
                                            管理资源管理器右键菜单「用 copywhere 发送」
  copywhere shell-send 文件/目录...         右键菜单调用：交给正在运行的实例发送
  copywhere clip-test                       本机剪贴板读写自检
  copywhere doctor                          环境自检（权限/配置/端口/托盘/菜单/日志）
  copywhere version                         显示版本号

工作方式:
  - 直接双击 copywhere.exe = gui 模式（隐藏控制台窗口，托盘常驻 +
    浏览器自动打开控制面板；命令行终端里执行时不隐藏，便于查看日志）
  - 各节点通过 UDP 广播自动发现彼此，无需配置 IP；同一节点多网卡地址自动去重
  - 配对认证：在 gui 面板「在线节点」点击「配对」，对端在面板确认后即完成，
    双方交换独立配对令牌（peers.json），可随时解除配对；
    未配对的节点无法传输（共享 token 认证已废弃）
  - run 运行期间，复制（Ctrl+C）小于阈值的文件或文本，会自动推送到所有在线节点
  - 对端收到文件后保存到接收目录并自动写回其剪贴板，直接 Ctrl+V 即可
  - 在资源管理器里右键文件/文件夹 →「用 copywhere 发送」= 立即发送，
    不受阈值限制；该项由 gui 启动时注册（可在面板「设置 → 通用」关闭）
  - run 默认启用三页终端界面（1 日志 / 2 在线节点 / 3 文件记录），
    -plain 或输出非终端时退化为纯日志
  - gui 同一进程内启动服务与系统托盘，并在浏览器打开控制面板
    （传输动态/节点/日志/设置），面板只监听 127.0.0.1

配置文件: ~/.copywhere/config.json（接收目录: ~/.copywhere/files）
`)
}

func main() {
	log.SetFlags(log.Ltime)
	// 无参数（资源管理器双击 exe）→ 直接进入 gui 模式
	if len(os.Args) < 2 {
		hideOwnConsole()
		if err := cmdGui(nil); err != nil {
			guiFatal(err)
		}
		return
	}
	// shell-send 由资源管理器右键菜单拉起：通常没有可读的控制台，结果用消息框呈现；
	// 从终端手动调用时仍打印到该终端（便于排查）。必须先隐藏自建控制台，
	// 开发构建（控制台子系统）直接启动才不会闪一个黑框。
	if os.Args[1] == "shell-send" {
		hideOwnConsole()
		haveConsole := !consoleHidden
		if consoleHidden {
			// windowsgui 构建从命令行调用时，挂回调用方终端以便输出
			haveConsole = attachParentConsole()
		}
		os.Exit(cmdShellSend(os.Args[2:], haveConsole))
	}
	attachParentConsole()
	var err error
	switch os.Args[1] {
	case "init":
		err = cmdInit(os.Args[2:])
	case "run":
		err = cmdRun(os.Args[2:])
	case "gui":
		err = cmdGui(os.Args[2:])
	case "nodes":
		err = cmdNodes(os.Args[2:])
	case "send":
		err = cmdSend(os.Args[2:])
	case "shell":
		err = cmdShell(os.Args[2:])
	case "shell-send":
		// 正常路径在 main 开头就分流了；保留分支只为让 switch 覆盖全部子命令
		os.Exit(cmdShellSend(os.Args[2:], true))
	case "clip-test":
		err = cmdClipTest(os.Args[2:])
	case "doctor":
		err = cmdDoctor(os.Args[2:])
	case "version", "-v", "--version":
		fmt.Printf("copywhere %s (%s/%s)\n", app.Version, runtime.GOOS, runtime.GOARCH)
		return
	case "help", "-h", "--help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "未知命令: %s\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		log.Fatalf("错误: %v", err)
	}
}

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	configPath := fs.String("config", "", "配置文件路径")
	fs.Parse(args)
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	path := *configPath
	if path == "" {
		path = config.DefaultPath()
	}
	log.Printf("配置: %s（节点 %q，面板端口 %d）", path, cfg.NodeName, cfg.WebPort)
	fmt.Println("配置已就绪:")
	fmt.Printf("  文件       : %s\n", path)
	fmt.Printf("  节点名     : %s\n", cfg.NodeName)
	fmt.Println("  认证       : 配对制——在 gui 面板「屏幕布局/在线节点」中配对后即可互通")
	fmt.Printf("  接收目录   : %s\n", cfg.ReceiveDir)
	fmt.Printf("  自动阈值   : %d MB\n", cfg.MaxAutoCopyMB)
	fmt.Printf("  发现/传输  : %d/udp, %d/tcp\n", cfg.DiscoveryPort, cfg.TransferPort)
	return nil
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	configPath := fs.String("config", "", "配置文件路径")
	plain := fs.Bool("plain", false, "纯日志模式（不用三页终端界面）")
	fs.Parse(args)
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	interactive := !*plain && term.IsTerminal(int(os.Stdout.Fd()))
	return app.Run(ctx, cfg, app.Options{Interactive: interactive})
}

func cmdGui(args []string) (err error) {
	// windowsgui 构建没有控制台，日志必须落文件才有排障依据（gui.log）。
	defer setupGUILogging()()
	// 任何退出原因都落盘：否则进程无声消失，用户只能看到"双击没反应"。
	// （消息框会被忽略或一闪而过，日志才是排障依据。）
	defer func() {
		if r := recover(); r != nil {
			log.Printf("gui 崩溃: %v\n%s", r, debug.Stack())
			err = fmt.Errorf("内部错误: %v（详情见 %s）", r, guiLogName)
		}
		if err != nil {
			log.Printf("gui 启动/运行失败: %v", err)
		}
	}()

	// 进程处于低完整性级别时，程序"看着在跑，其实全废"：写不了用户目录、
	// 托盘图标会被资源管理器丢掉、鼠标也注入不了。此时明确拒绝启动并说清原因，
	// 否则只会留下一个占着端口和单实例锁的僵尸实例，让后面每次双击都失败。
	if app.LowIntegrity() {
		return fmt.Errorf("%s", app.SandboxedHint())
	}

	fs := flag.NewFlagSet("gui", flag.ExitOnError)
	configPath := fs.String("config", "", "配置文件路径")
	fs.Parse(args)

	if !acquireSingleton() {
		// 已有实例在跑（含异常残留的进程）：直接把它的控制面板打开再安静退出。
		// 否则用户双击得到的只是一句"已有另一个实例"，看着就像程序坏了。
		if url, uerr := webui.ReadPanelURL(*configPath); uerr == nil {
			if oerr := webui.OpenBrowserChecked(url); oerr == nil {
				log.Printf("已有 copywhere 实例在运行，已打开它的控制面板（无需重复启动）")
				return nil
			}
		}
		return fmt.Errorf("已有另一个 copywhere 实例在运行（gui 与 run 不能同时启动）")
	}
	defer releaseSingleton()

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	path := *configPath
	if path == "" {
		path = config.DefaultPath()
	}
	log.Printf("配置: %s（节点 %q，面板端口 %d）", path, cfg.NodeName, cfg.WebPort)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	bus := webui.NewBus()
	// LogFile 只在成功打开日志文件时传入：*os.File 的 nil 值装进 io.Writer
	// 会变成非 nil 接口，反而让 MultiWriter 写一个空句柄。
	opts := app.Options{GUI: true, Sink: bus}
	if guiLogFile != nil {
		opts.LogFile = guiLogFile
	}
	a, err := app.Start(ctx, cfg, opts)
	if err != nil {
		// 端口被占用 = 多半有另一个实例在跑（可能是别的会话/权限下启动、托盘看不见的）。
		// 能连上它的面板就直接打开，别让用户对着一个报错框猜。
		var pe *app.PortInUseError
		if errors.As(err, &pe) {
			if url, uerr := webui.ReadPanelURL(path); uerr == nil {
				if oerr := webui.OpenBrowserChecked(url); oerr == nil {
					log.Printf("%v，已打开既有实例的控制面板（无需重复启动）", pe)
					return nil
				}
			}
			log.Printf("端口冲突且读不到既有实例的面板地址，无法复用：%v", pe)
		}
		return err
	}
	log.Printf("服务已就绪：发现 %d/udp，传输 %d/tcp，KVM %d/tcp", cfg.DiscoveryPort, cfg.TransferPort, cfg.KVMPort)
	// 资源管理器右键菜单：默认开启，启动时确保注册项指向当前 exe
	//（程序被移动/升级后自动纠正；关闭时顺手清理残留项）。
	// 异步执行：注册表写入可能被杀软拦截甚至长时间挂起，同步做会把托盘图标
	// 一起拖死（表现为"进程在、托盘没出来"）。
	// 开关值在起 goroutine 之前取：面板保存会在另一 goroutine 里改写 cfg。
	menuOn := cfg.ShellMenuOn()
	go func() {
		if err := a.SetShellMenu(menuOn); err != nil {
			log.Printf("同步资源管理器右键菜单失败: %v", err)
		}
	}()
	srv, err := webui.NewServer(a, cfg, path, bus)
	if err != nil {
		return err
	}
	defer srv.Shutdown()
	srv.StartSnapshotLoop(ctx)
	log.Printf("控制面板: %s", srv.URL())
	// 面板地址（含访问令牌）落到配置旁文件：浏览器未自动打开时可手动查阅，
	// 资源管理器右键菜单的 shell-send 也靠它定位本实例。
	urlFile := webui.PanelURLPath(path)
	if werr := os.WriteFile(urlFile, []byte(srv.URL()), 0o600); werr != nil {
		log.Printf("记录面板地址失败: %v", werr)
	}
	defer os.Remove(urlFile)
	// 提权环境（例如从管理员权限的资源管理器双击启动）会自动拉起提权浏览器实例，
	// 之后系统会弹「现有实例正在以提升的权限运行」并把浏览器卡死，故先检测再打开。
	if err := webui.OpenBrowserChecked(srv.URL()); err != nil {
		log.Printf("未自动打开浏览器：%v（面板地址已写入 %s，可手动访问）", err, urlFile)
	}
	webui.RunTray(webui.TrayOptions{
		OpenPanel:      func() { webui.OpenBrowser(srv.URL()) },
		OpenReceiveDir: func() { webui.RevealInExplorer(cfg.ReceiveDir, false) },
		IsPaused:       a.IsPaused,
		SetPaused:      a.SetPaused,
		Quit:           stop,
	})
	return a.Wait()
}

// acquireSingleton 用命名互斥锁保证 gui 与 run 不会双实例运行
// （剪贴板监控与端口都无法被两个实例共享）。
var muHandle windows.Handle

func acquireSingleton() bool {
	name, err := windows.UTF16PtrFromString(`Local\copywhere-singleton`)
	if err != nil {
		return false
	}
	h, err := windows.CreateMutex(nil, false, name)
	if err != nil {
		if h != 0 {
			windows.CloseHandle(h) // 已有实例持有（ERROR_ALREADY_EXISTS）
		}
		return false
	}
	muHandle = h
	return true
}

func releaseSingleton() {
	if muHandle != 0 {
		windows.ReleaseMutex(muHandle)
		windows.CloseHandle(muHandle)
		muHandle = 0
	}
}

// gui 模式无可见控制台时，致命错误用消息框呈现，避免静默退出。
var consoleHidden bool

// attachParentConsole 当以命令行子命令（如 init/run/nodes）启动时，尝试附加到调用方的终端控制台。
// 返回是否成功附加（调用方据此决定输出到终端还是弹消息框）。
func attachParentConsole() bool {
	k32 := windows.NewLazySystemDLL("kernel32.dll")
	attachConsole := k32.NewProc("AttachConsole")
	r, _, _ := attachConsole.Call(^uintptr(0)) // ATTACH_PARENT_PROCESS = (DWORD)-1
	if r == 0 {
		return false
	}
	hOut, _ := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE)
	hErr, _ := windows.GetStdHandle(windows.STD_ERROR_HANDLE)
	hIn, _ := windows.GetStdHandle(windows.STD_INPUT_HANDLE)
	if hOut != 0 && hOut != windows.InvalidHandle {
		os.Stdout = os.NewFile(uintptr(hOut), "/dev/stdout")
	}
	if hErr != 0 && hErr != windows.InvalidHandle {
		os.Stderr = os.NewFile(uintptr(hErr), "/dev/stderr")
	}
	if hIn != 0 && hIn != windows.InvalidHandle {
		os.Stdin = os.NewFile(uintptr(hIn), "/dev/stdin")
	}
	return true
}

// hideOwnConsole 隐藏双击启动时系统为本进程自动创建的控制台窗口（或在 windowsgui 模式下标记无控制台）。
func hideOwnConsole() {
	k32 := windows.NewLazySystemDLL("kernel32.dll")
	user32 := windows.NewLazySystemDLL("user32.dll")
	getConsoleWindow := k32.NewProc("GetConsoleWindow")
	getConsoleProcessList := k32.NewProc("GetConsoleProcessList")
	showWindow := user32.NewProc("ShowWindow")
	freeConsole := k32.NewProc("FreeConsole")
	hwnd, _, _ := getConsoleWindow.Call()
	if hwnd == 0 {
		// windowsgui 子系统启动时默认无控制台窗口
		consoleHidden = true
		return
	}
	var pids [2]uint32
	n, _, _ := getConsoleProcessList.Call(
		uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	if n <= 1 {
		showWindow.Call(hwnd, 0 /* SW_HIDE */)
		freeConsole.Call()
		consoleHidden = true
	}
}

// guiFatal 在 gui 模式下报告启动失败：有终端时输出日志，
// 控制台已隐藏（双击启动）时弹出消息框，两者都做兜底。
func guiFatal(err error) {
	log.Printf("错误: %v", err)
	if consoleHidden {
		messageBox("copywhere", "copywhere 启动失败：\n\n"+err.Error())
	}
	os.Exit(1)
}

func cmdNodes(args []string) error {
	fs := flag.NewFlagSet("nodes", flag.ExitOnError)
	configPath := fs.String("config", "", "配置文件路径")
	wait := fs.Duration("wait", 4*time.Second, "扫描等待时长")
	fs.Parse(args)
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), *wait)
	defer cancel()
	peers := app.ScanPeers(ctx, cfg)
	if len(peers) == 0 {
		fmt.Println("未发现其他在线节点（确认对端已运行 copywhere，且已在面板完成配对、防火墙放行）")
		return nil
	}
	fmt.Printf("发现 %d 个在线节点:\n", len(peers))
	for _, p := range peers {
		fmt.Printf("  [远程]%s(%s)\n", p.Name, strings.Join(p.IPs, ", "))
	}
	return nil
}

func cmdSend(args []string) error {
	fs := flag.NewFlagSet("send", flag.ExitOnError)
	configPath := fs.String("config", "", "配置文件路径")
	text := fs.String("text", "", "发送文本而不是文件")
	wait := fs.Duration("wait", 4*time.Second, "发现节点等待时长")
	fs.Parse(args)
	paths := fs.Args()
	if *text == "" && len(paths) == 0 {
		return fmt.Errorf("请指定要发送的文件/目录，或用 -text 指定文本")
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("无法访问 %s: %w", p, err)
		}
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), *wait)
	defer cancel()
	peers := app.ScanPeers(ctx, cfg)
	if len(peers) == 0 {
		return fmt.Errorf("未发现在线节点（等待 %s）", *wait)
	}
	log.Printf("发现 %d 个在线节点，开始发送", len(peers))
	return app.SendToPeers(cfg, peers, paths, *text)
}

// cmdShell 管理资源管理器右键菜单（默认由 gui 启动时自动注册）。
func cmdShell(args []string) error {
	fs := flag.NewFlagSet("shell", flag.ExitOnError)
	install := fs.Bool("install", false, "注册资源管理器右键菜单")
	uninstall := fs.Bool("uninstall", false, "移除资源管理器右键菜单")
	// -status 与无参数同义：只为让它出现在 help 里，取值无需区分
	fs.Bool("status", false, "查看当前注册状态（无参数时的默认行为）")
	fs.Parse(args)

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("定位当前程序路径失败: %w", err)
	}
	switch {
	case *install && *uninstall:
		return fmt.Errorf("-install 与 -uninstall 只能选一个")
	case *install:
		if err := shellmenu.Install(exe); err != nil {
			return err
		}
		fmt.Printf("已注册右键菜单「%s」→ %s\n", shellmenu.MenuText, exe)
		fmt.Println("在资源管理器中选中文件/文件夹右键即可看到（已打开的窗口可能需重开）")
		return nil
	case *uninstall:
		if err := shellmenu.Uninstall(); err != nil {
			return err
		}
		fmt.Printf("已移除右键菜单「%s」\n", shellmenu.MenuText)
		return nil
	}
	// 到这里只剩"-status 或未指定动作"：统一查询并展示当前注册状态
	installed := shellmenu.Installed("")
	if !installed {
		fmt.Printf("右键菜单「%s」：未注册（gui 启动时会自动注册）\n", shellmenu.MenuText)
		return nil
	}
	fmt.Printf("右键菜单「%s」：已注册\n", shellmenu.MenuText)
	if !shellmenu.Installed(exe) {
		fmt.Println("注意：菜单指向的不是当前 exe（程序被移动或升级过），运行 copywhere shell -install 可纠正")
	}
	fmt.Printf("当前命令行: %s\n", shellmenu.Command(exe))
	return nil
}

// cmdShellSend 是资源管理器右键菜单的落地动作：把选中路径交给正在运行的实例发送。
// 返回进程退出码；失败原因走消息框（右键拉起没有控制台）或终端（从命令行手动调用）。
func cmdShellSend(args []string, haveConsole bool) int {
	report := func(title, text string) {
		if haveConsole {
			fmt.Fprintf(os.Stderr, "%s: %s\n", title, text)
			return
		}
		messageBox(title, text)
	}
	fs := flag.NewFlagSet("shell-send", flag.ContinueOnError)
	configPath := fs.String("config", "", "配置文件路径")
	fs.SetOutput(io.Discard) // 无控制台可写，解析错误也不该往 stderr 刷
	if err := fs.Parse(args); err != nil && !errors.Is(err, flag.ErrHelp) {
		report("copywhere 右键发送", "参数无法解析："+err.Error())
		return 2
	}
	paths := fs.Args()
	if len(paths) == 0 {
		report("copywhere 右键发送",
			"没有收到要发送的文件。\n\n请用资源管理器右键菜单的「"+shellmenu.MenuText+"」，或先选中文件再右键。")
		return 2
	}
	if err := webui.SendPathsViaPanel(*configPath, paths); err != nil {
		report("copywhere 右键发送失败", shellSendHint(err))
		return 1
	}
	return 0
}

// shellSendHint 把 helper 的失败原因补成用户能照做的提示。
func shellSendHint(err error) string {
	if !errors.Is(err, webui.ErrPanelNotRunning) {
		return err.Error()
	}
	if singletonHeld() {
		// 有实例占着单实例锁、却读不到面板地址：终端 run 模式（没有面板），
		// 或用 -config 指向了另一个配置文件（地址文件在它旁边）
		return "已有一个 copywhere 实例在运行，但找不到它的控制面板。\n\n" +
			"终端 run 模式没有面板，右键发送需要 gui 模式：请退出该实例，改为双击 copywhere.exe 启动。\n" +
			"若用 -config 指定了其他配置文件，请把该实例改为默认配置（~/.copywhere/config.json）。"
	}
	return "copywhere 没有在运行，无法发送文件。\n\n" +
		"请先双击 copywhere.exe（托盘常驻）后再右键发送。\n\n（" + err.Error() + "）"
}

// singletonHeld 报告本机是否已有 copywhere 实例（gui 或 run）在运行。
func singletonHeld() bool {
	name, err := windows.UTF16PtrFromString(`Local\copywhere-singleton`)
	if err != nil {
		return false
	}
	h, err := windows.CreateMutex(nil, false, name)
	if h != 0 {
		defer windows.CloseHandle(h)
	}
	return err != nil
}

// messageBox 弹出系统消息框：gui / 右键菜单这类没有终端的入口用它呈现错误。
func messageBox(title, text string) {
	t, terr := windows.UTF16PtrFromString(text)
	cap, _ := windows.UTF16PtrFromString(title)
	if terr != nil {
		return
	}
	user32 := windows.NewLazySystemDLL("user32.dll")
	// MB_ICONERROR | MB_SETFOREGROUND
	user32.NewProc("MessageBoxW").Call(0,
		uintptr(unsafe.Pointer(t)), uintptr(unsafe.Pointer(cap)), 0x10|0x10000)
}

func cmdClipTest(args []string) error {
	fs := flag.NewFlagSet("clip-test", flag.ExitOnError)
	fs.Parse(args)
	fmt.Println("注意：测试会临时改写并尝试恢复你的剪贴板。")

	prevFiles, _ := clip.ReadFiles()
	prevText, _ := clip.ReadText()

	msg := fmt.Sprintf("copywhere-selftest-%d", time.Now().UnixNano())
	if err := clip.SetText(msg); err != nil {
		return fmt.Errorf("SetText: %w", err)
	}
	got, err := clip.ReadText()
	if err != nil || got != msg {
		return fmt.Errorf("文本回读不一致: %q, err=%v", got, err)
	}
	fmt.Println("文本读写       : OK")

	tmp, err := os.CreateTemp("", "copywhere-test-*.txt")
	if err != nil {
		return err
	}
	tmp.WriteString("copywhere clip test")
	tmp.Close()
	defer os.Remove(tmp.Name())

	if err := clip.SetFiles([]string{tmp.Name()}); err != nil {
		return fmt.Errorf("SetFiles: %w", err)
	}
	files, err := clip.ReadFiles()
	if err != nil || len(files) != 1 || files[0] != tmp.Name() {
		return fmt.Errorf("文件回读不一致: %v, err=%v", files, err)
	}
	fmt.Println("文件列表读写   : OK")

	if len(prevFiles) > 0 {
		clip.SetFiles(prevFiles)
	} else if prevText != "" {
		clip.SetText(prevText)
	}
	fmt.Println("剪贴板自检通过")
	return nil
}
