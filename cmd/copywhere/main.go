// copywhere — 局域网剪贴板/文件自动同步工具（Windows）。
//
// 在一台机器上 Ctrl+C 复制文件（小于阈值，默认 10MB）或文本，
// 局域网内所有在线节点的剪贴板都会出现同样的内容，直接 Ctrl+V 即可。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/term"

	"copywhere/internal/app"
	"copywhere/internal/clip"
	"copywhere/internal/config"
	"copywhere/internal/webui"
)

func usage() {
	fmt.Print(`copywhere — 局域网剪贴板/文件自动同步工具

用法:
  copywhere init                            生成默认配置（含随机 token）
  copywhere run [-config 路径] [-plain]     启动服务（节点发现 + 传输 + 剪贴板监控）
  copywhere gui [-config 路径]              启动服务 + 系统托盘 + 浏览器控制面板
  copywhere nodes [-config 路径] [-wait 4s] 扫描并列出局域网在线节点
  copywhere send [-config 路径] [-text 内容] [-wait 4s] 文件/目录...
                                            立即发送到所有在线节点（不受大小阈值限制）
  copywhere clip-test                       本机剪贴板读写自检
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
	case "clip-test":
		err = cmdClipTest(os.Args[2:])
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

func cmdGui(args []string) error {
	fs := flag.NewFlagSet("gui", flag.ExitOnError)
	configPath := fs.String("config", "", "配置文件路径")
	fs.Parse(args)

	if !acquireSingleton() {
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	bus := webui.NewBus()
	a, err := app.Start(ctx, cfg, app.Options{GUI: true, Sink: bus})
	if err != nil {
		return err
	}
	srv, err := webui.NewServer(a, cfg, path, bus)
	if err != nil {
		return err
	}
	defer srv.Shutdown()
	srv.StartSnapshotLoop(ctx)
	log.Printf("控制面板: %s", srv.URL())
	// 面板地址（含访问令牌）落到配置旁文件，浏览器未自动打开时可手动查阅
	urlFile := filepath.Join(filepath.Dir(path), "panel_url.txt")
	if werr := os.WriteFile(urlFile, []byte(srv.URL()), 0o600); werr != nil {
		log.Printf("记录面板地址失败: %v", werr)
	}
	defer os.Remove(urlFile)
	if err := webui.OpenBrowser(srv.URL()); err != nil {
		log.Printf("自动打开浏览器失败（可手动访问上方地址）: %v", err)
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
func attachParentConsole() {
	k32 := windows.NewLazySystemDLL("kernel32.dll")
	attachConsole := k32.NewProc("AttachConsole")
	r, _, _ := attachConsole.Call(^uintptr(0)) // ATTACH_PARENT_PROCESS = (DWORD)-1
	if r != 0 {
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
	}
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
		text, terr := windows.UTF16PtrFromString("copywhere 启动失败：\n\n" + err.Error())
		title, _ := windows.UTF16PtrFromString("copywhere")
		if terr == nil {
			user32 := windows.NewLazySystemDLL("user32.dll")
			// MB_ICONERROR | MB_SETFOREGROUND
			user32.NewProc("MessageBoxW").Call(0,
				uintptr(unsafe.Pointer(text)), uintptr(unsafe.Pointer(title)),
				0x10|0x10000)
		}
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
		fmt.Println("未发现其他在线节点（确认对端已运行 copywhere run，且 token 一致、防火墙放行）")
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
