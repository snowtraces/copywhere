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
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"copywhere/internal/app"
	"copywhere/internal/clip"
	"copywhere/internal/config"
	"copywhere/internal/discovery"
	"copywhere/internal/ipc"
	"copywhere/internal/shellmenu"
)

// buildFlavor 区分同一份代码的两个产物：
//   - "gui"：GUI 子系统（`-H windowsgui`），双击不弹控制台，只负责桌面窗口；
//     带子命令时按命令行客户端执行（单文件）。
//   - "cli"（默认，仅开发调试用）：控制台子系统，便于看日志。
//
// 由 -ldflags "-X main.buildFlavor=gui" 注入。
var buildFlavor = "cli"

func usage() {
	fmt.Print(`copywhere — 局域网剪贴板/文件自动同步工具

用法（单文件即可）:
  copywhere.exe <子命令>            命令行（同一个 exe；窗口实例在跑时经本机管道
                                    委派给它执行，并等待每个对端的结果）
  copywhere.exe                     双击 = 桌面窗口（GUI 子系统，不弹控制台）

提示：PowerShell 里脚本请用管道，例如
      copywhere.exe nodes | Out-File n.txt
      裸调用与 > 重定向对 GUI 子系统进程不可靠（原因见 README / docs）。

子命令:
  copywhere init                            生成默认配置
  copywhere run [-config 路径]              启动无窗口服务（日志到标准输出，适合服务器 / SSH）
  copywhere wails [-config 路径]            启动服务 + 系统托盘 + Wails v3 桌面窗口
                                            （WebView2；无参数双击 exe 即进入该模式）
  copywhere nodes [-config 路径] [-wait 4s] 列出在线节点（有实例运行时直接查询该实例）
  copywhere send [-config 路径] [-text 内容] [-wait 4s] [-no-wait] 文件/目录...
                                            立即发送到所有在线节点（不受大小阈值限制；
                                            有实例运行时交给该实例发送并等待结果汇总）
  copywhere status <任务id>                 查询 send 委派给窗口实例的任务结果
  copywhere shell [-install|-uninstall|-status]
                                            管理资源管理器右键菜单「用 copywhere 发送」
  copywhere shell-send 文件/目录...         右键菜单调用：交给正在运行的实例发送
  copywhere clip-test                       本机剪贴板读写自检
  copywhere doctor                          环境自检（权限/配置/端口/WebView2/菜单/日志）
  copywhere version                         显示版本号

工作方式:
  - 直接双击 copywhere.exe = 桌面窗口模式（隐藏控制台窗口，托盘常驻 +
    WebView2 窗口；关闭窗口收进托盘，二次启动唤回已有窗口）
  - 各节点通过 UDP 广播自动发现彼此，无需配置 IP；同一节点多网卡地址自动去重
  - 配对认证：在窗口「在线节点」点击「配对」，对端确认后即完成，
    双方交换独立配对令牌（peers.json），可随时解除配对；
    未配对的节点无法传输（共享 token 认证已废弃）
  - run 运行期间，复制（Ctrl+C）小于阈值的文件或文本，会自动推送到所有在线节点
  - 对端收到文件后保存到接收目录并自动写回其剪贴板，直接 Ctrl+V 即可
  - 在资源管理器里右键文件/文件夹 →「用 copywhere 发送」= 立即发送，
    不受阈值限制；该项由桌面窗口启动时注册（可在窗口「设置 → 通用」关闭）
  - 桌面窗口运行时，nodes / send / shell-send 都通过本机命名管道交给它处理
    （发现端口与配对状态都在该进程里，子进程不再重复监听）；send 默认等待
    每个对端的投递结果并汇总，脚本可用 | Out-File 取结果（不要用 >，见文档）

配置文件: ~/.copywhere/config.json（接收目录: ~/.copywhere/files）
`)
}

func main() {
	log.SetFlags(log.Ltime)
	// 单文件设计：同一个 exe 既是桌面窗口，也是命令行客户端。
	// 带子命令时按普通 CLI 流程执行（窗口实例在跑时经命名管道委派给
	// 该实例并等待结果，见 sendViaInstance）。

	// 无参数（资源管理器双击 exe）→ 桌面前端（Wails 窗口）。
	if len(os.Args) < 2 {
		hideOwnConsole()
		if err := cmdWails(nil); err != nil {
			uiFatal(err)
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
	case "wails":
		err = cmdWails(os.Args[2:])
	case "nodes":
		err = cmdNodes(os.Args[2:])
	case "send":
		err = cmdSend(os.Args[2:])
	case "status":
		err = cmdStatus(os.Args[2:])
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
	log.Printf("配置: %s（节点 %q）", path, cfg.NodeName)
	fmt.Println("配置已就绪:")
	fmt.Printf("  文件       : %s\n", path)
	fmt.Printf("  节点名     : %s\n", cfg.NodeName)
	fmt.Println("  认证       : 配对制——在窗口「屏幕布局/在线节点」中配对后即可互通")
	fmt.Printf("  接收目录   : %s\n", cfg.ReceiveDir)
	fmt.Printf("  自动阈值   : %d MB\n", cfg.MaxAutoCopyMB)
	fmt.Printf("  发现/传输  : %d/udp, %d/tcp\n", cfg.DiscoveryPort, cfg.TransferPort)
	return nil
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	configPath := fs.String("config", "", "配置文件路径")
	fs.Parse(args)
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return app.Run(ctx, cfg, app.Options{})
}

// acquireSingleton 用命名互斥锁保证桌面窗口与 run 不会双实例运行
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

// 桌面窗口模式无可见控制台时，致命错误用消息框呈现，避免静默退出。
var consoleHidden bool

// attachParentConsole 当以命令行子命令（如 init/run/nodes）启动时，尝试附加到调用方的终端控制台。
// 返回是否已有可用控制台（调用方据此决定输出到终端还是弹消息框）。
//
// 注意：cmd / PowerShell / 管道已经把标准句柄交给本进程时，**不要**再 AttachConsole——
// 那会把 os.Stdout 换成 CONOUT$，`copywhere nodes > out.txt`、`... | Out-String`
// 这类重定向/管道会拿不到任何输出，看起来就像"命令没有反应"。
func attachParentConsole() bool {
	if stdHandleUsable(windows.STD_OUTPUT_HANDLE) || stdHandleUsable(windows.STD_ERROR_HANDLE) {
		return true
	}
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

// stdHandleUsable 报告某个标准句柄是否已经可用（控制台/管道/文件重定向都算）。
func stdHandleUsable(which uint32) bool {
	h, err := windows.GetStdHandle(which)
	return err == nil && h != 0 && h != windows.InvalidHandle
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

// uiFatal 在桌面前端报告启动失败：有终端时输出日志，
// 控制台已隐藏（双击启动）时弹出消息框，两者都做兜底。
func uiFatal(err error) {
	log.Printf("错误: %v", err)
	if consoleHidden {
		messageBox("copywhere", "copywhere 启动失败：\n\n"+err.Error())
	}
	os.Exit(1)
}

func cmdNodes(args []string) error {
	fs := flag.NewFlagSet("nodes", flag.ExitOnError)
	configPath := fs.String("config", "", "配置文件路径")
	wait := fs.Duration("wait", 4*time.Second, "扫描等待时长（仅在无运行中实例时生效）")
	fs.Parse(args)
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	// 桌面窗口（或 run）在运行时会独占发现端口，本进程再扫描必然绑定失败——
	// 早期版本把这个错误吞掉，只打印"未发现在线节点"，看起来就像命令失效。
	// 现在直接向运行中的实例要节点表（它才是真正在收公告的那个进程）。
	if ipc.Ping() == nil {
		var peers []discovery.Peer
		if err := ipc.SendInto(ipc.Command{Cmd: ipc.CmdPeers}, &peers); err != nil {
			return fmt.Errorf("向运行中的实例查询在线节点失败: %w", err)
		}
		printPeers(peers, "（来自运行中的 copywhere 实例）")
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), *wait)
	defer cancel()
	peers, err := app.ScanPeers(ctx, cfg)
	if err != nil {
		return fmt.Errorf("%w\n提示：发现端口 %d/udp 被其它程序占用时无法扫描，请关闭占用者后重试", err, cfg.DiscoveryPort)
	}
	printPeers(peers, "")
	return nil
}

// printPeers 打印节点表；note 用于说明数据来源。
func printPeers(peers []discovery.Peer, note string) {
	if len(peers) == 0 {
		fmt.Println("未发现其他在线节点（确认对端已运行 copywhere、在同一网段、防火墙放行）" + note)
		return
	}
	fmt.Printf("发现 %d 个在线节点%s:\n", len(peers), note)
	for _, p := range peers {
		fmt.Printf("  [远程]%s(%s)\n", p.Name, strings.Join(p.IPs, ", "))
	}
}

func cmdSend(args []string) error {
	fs := flag.NewFlagSet("send", flag.ExitOnError)
	configPath := fs.String("config", "", "配置文件路径")
	text := fs.String("text", "", "发送文本而不是文件")
	wait := fs.Duration("wait", 4*time.Second, "发现节点等待时长（仅在无运行中实例时生效）")
	noWait := fs.Bool("no-wait", false, "委派给运行中实例时立即返回，不等待投送结果")
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

	// 有实例在跑时交给它发送：节点表、配对令牌、失败重试都在那个进程里，
	// 本进程自己扫描会因发现端口被占用而失败（历史上 send 因此"没反应"）。
	// 默认等待任务完成并按每个对端的结果汇总；-no-wait 只建任务不等待。
	if ipc.Ping() == nil {
		return sendViaInstance(paths, *text, *noWait)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *wait)
	defer cancel()
	peers, err := app.ScanPeers(ctx, cfg)
	if err != nil {
		return fmt.Errorf("%w\n提示：发现端口 %d/udp 被其它程序占用时无法扫描，请关闭占用者后重试", err, cfg.DiscoveryPort)
	}
	if len(peers) == 0 {
		return fmt.Errorf("未发现在线节点（等待 %s）", *wait)
	}
	log.Printf("发现 %d 个在线节点，开始发送", len(peers))
	return app.SendToPeers(cfg, peers, paths, *text)
}

// cmdStatus 查询委派给 GUI 实例的任务（send 默认会打印 id）。
func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	fs.Parse(args)
	id := fs.Arg(0)
	if id == "" {
		return fmt.Errorf("用法: copywhere status <任务id>（id 由 send 输出）")
	}
	var st jobStatusReply
	if err := ipc.SendInto(ipc.Command{Cmd: ipc.CmdJob, ID: id}, &st); err != nil {
		return fmt.Errorf("查询任务失败: %w", err)
	}
	if st.State == "running" {
		fmt.Printf("任务 %s（%s）仍在进行\n", st.ID, st.Kind)
		return nil
	}
	fmt.Printf("任务 %s（%s）已完成\n", st.ID, st.Kind)
	return printJobResult(st)
}

// ---------- 命令行委派（交给运行中的 GUI 实例执行） ----------

// jobStartReply 是 IPC 建任务的返回。
type jobStartReply struct {
	ID string `json:"id"`
}

// jobStatusReply 是任务快照。
type jobStatusReply struct {
	ID      string          `json:"id"`
	Kind    string          `json:"kind"`
	State   string          `json:"state"`
	Results []jobPeerResult `json:"results"`
}

// sendViaInstance 把投送请求交给运行中的实例，默认等待并汇总每个对端的结果。
//
// 轮询而不是让管道阻塞等待：管理道是单实例串行的，一次大文件传输若占着连接，
// 右键发送/二次启动/其它命令都会被堵住。任务在 GUI 侧异步执行，
// 这里只发毫秒级的"查状态"请求。
func sendViaInstance(paths []string, text string, noWait bool) error {
	req := ipc.Command{Cmd: ipc.CmdSendJob, Paths: paths}
	if text != "" {
		req = ipc.Command{Cmd: ipc.CmdSendText, Text: text}
	}
	var started jobStartReply
	if err := ipc.SendInto(req, &started); err != nil {
		return fmt.Errorf("交给运行中的实例发送失败: %w", err)
	}
	if started.ID == "" {
		return fmt.Errorf("运行中的实例没有返回任务 id")
	}
	if noWait {
		fmt.Printf("已受理，任务 %s（用 copywhere status %s 查看）\n", started.ID, started.ID)
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	for {
		var st jobStatusReply
		if err := ipc.SendInto(ipc.Command{Cmd: ipc.CmdJob, ID: started.ID}, &st); err != nil {
			return fmt.Errorf("查询任务 %s 失败: %w", started.ID, err)
		}
		if st.State != "running" {
			return printJobResult(st)
		}
		select {
		case <-ctx.Done():
			fmt.Printf("已脱离等待，投送在窗口实例中继续（任务 %s）\n", started.ID)
			return nil
		case <-ticker.C:
		}
	}
}

// printJobResult 打印每个对端的结果并给出汇总；有致命失败时返回错误（退出码 1）。
// "重试" 表示对端暂不可达、已进入 GUI 的 60s 重试队列，不算失败。
func printJobResult(st jobStatusReply) error {
	if len(st.Results) == 0 {
		fmt.Println("任务完成：没有可投递的对端（可能已离线）")
		return nil
	}
	ok, retry, failed := 0, 0, 0
	for _, r := range st.Results {
		switch {
		case r.OK:
			ok++
			fmt.Printf("  [成功] %s\n", r.Peer)
		case r.Retry:
			retry++
			fmt.Printf("  [重试] %s：%s（60s 内自动补发）\n", r.Peer, r.Msg)
		default:
			failed++
			fmt.Printf("  [失败] %s：%s\n", r.Peer, r.Msg)
		}
	}
	fmt.Printf("汇总：成功 %d、重试 %d、失败 %d\n", ok, retry, failed)
	if failed > 0 {
		return fmt.Errorf("有 %d 个节点投送失败", failed)
	}
	return nil
}

// cmdShell 管理资源管理器右键菜单（默认由桌面窗口启动时自动注册）。
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
		fmt.Printf("右键菜单「%s」：未注册（桌面窗口启动时会自动注册）\n", shellmenu.MenuText)
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
	_ = fs.String("config", "", "配置文件路径（兼容旧菜单命令；IPC 为登录会话级）")
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
	if err := ipc.SendPaths(paths); err != nil {
		report("copywhere 右键发送失败", shellSendHint(err))
		return 1
	}
	return 0
}

// shellSendHint 把 helper 的失败原因补成用户能照做的提示。
func shellSendHint(err error) string {
	if !errors.Is(err, ipc.ErrNotRunning) {
		return err.Error()
	}
	if singletonHeld() {
		// 有实例占着单实例锁、但管道连不上：可能是终端 run 模式（不提供
		// 桌面窗口与 IPC 服务），或实例仍在启动中。
		return "已有一个 copywhere 实例在运行，但它没有提供桌面窗口 IPC。\n\n" +
			"右键发送需要桌面窗口实例：请退出该实例，改为双击 copywhere.exe 启动。\n" +
			"若刚启动，请稍等一秒再试。"
	}
	return "copywhere 没有在运行，无法发送文件。\n\n" +
		"请先双击 copywhere.exe（托盘常驻）后再右键发送。\n\n（" + err.Error() + "）"
}

// singletonHeld 报告本机是否已有 copywhere 实例（桌面窗口或 run）在运行。
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

// messageBox 弹出系统消息框：桌面窗口 / 右键菜单这类没有终端的入口用它呈现错误。
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
