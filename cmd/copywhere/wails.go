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

	"copywhere/internal/app"
	"copywhere/internal/config"
	"copywhere/internal/ipc"
	"copywhere/internal/panel"
	"copywhere/internal/wailsui"
)

// cmdWails 是 copywhere 唯一的图形入口：同一进程内运行完整服务（发现/传输/
// 剪贴板/KVM）+ Wails v3 桌面窗口（WebView2）+ 系统托盘 + 本地 IPC。
//
// 与 run（无窗口服务）共用命名互斥锁，避免两个实例抢剪贴板监听与端口。
func cmdWails(args []string) (err error) {
	fs := flag.NewFlagSet("wails", flag.ExitOnError)
	configPath := fs.String("config", "", "配置文件路径")
	smoke := fs.Duration("smoke", 0, "冒烟测试：窗口启动后等待该时长自动退出（如 5s）")
	fs.Parse(args)

	closeLog := setupGUILogging()
	defer closeLog()

	defer func() {
		if err != nil {
			log.Printf("桌面窗口启动/运行失败: %v", err)
		}
	}()

	// 低完整性进程写不了用户目录、托盘图标会被系统丢弃、输入注入也会被拒：
	// 此时"看着在跑、其实全废"，明确拒绝启动并说清原因。
	if app.LowIntegrity() {
		return fmt.Errorf("%s", app.SandboxedHint())
	}

	if *smoke == 0 {
		if !acquireSingleton() {
			// 已有实例：先通过命名管道请它把窗口拿到前台；管道不可用
			// （例如终端 run 模式）再退回窗口枚举。
			if aerr := ipc.Activate(); aerr == nil {
				log.Printf("已有 copywhere 实例在运行，已通过 IPC 唤回它的桌面窗口")
				return nil
			}
			if wailsui.ActivateExistingWindow() {
				log.Printf("已有 copywhere 实例在运行，已激活它的桌面窗口")
				return nil
			}
			return fmt.Errorf("已有另一个 copywhere 实例在运行（run 与 wails 不能同时启动）")
		}
		defer releaseSingleton()
	} else {
		// 冒烟测试用隔离配置与端口运行，允许与本机正在使用的实例并存。
		log.Printf("冒烟模式：跳过单实例检查（请配合 -config 指向隔离配置）")
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	path := *configPath
	if path == "" {
		path = config.DefaultPath()
	}
	log.Printf("配置: %s（节点 %q）", path, cfg.NodeName)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var logFile io.Writer
	if guiLogFile != nil {
		logFile = guiLogFile
	}

	bus := panel.NewBus()
	a, err := app.Start(ctx, cfg, app.Options{GUI: true, Sink: bus, LogFile: logFile})
	if err != nil {
		return err
	}

	// 本机 IPC：右键菜单 shell-send 与二次启动都靠它，不再需要 HTTP 面板。
	ipcSrv, ierr := ipc.Serve(func(cmd ipc.Command) error {
		switch cmd.Cmd {
		case ipc.CmdPing:
			return nil
		case ipc.CmdActivate:
			if wailsui.ActivateExistingWindow() {
				return nil
			}
			return errors.New("窗口尚未就绪，请稍后再试")
		case ipc.CmdSend:
			return a.ShellSendChecked(cmd.Paths)
		default:
			return fmt.Errorf("未知命令: %q", cmd.Cmd)
		}
	})
	if ierr != nil {
		log.Printf("本机 IPC 启动失败（右键发送/二次唤起将不可用）: %v", ierr)
	} else {
		defer ipcSrv.Close()
	}

	if *smoke == 0 {
		// 资源管理器右键菜单：启动时确保注册项指向当前 exe（程序移动/升级后自动纠正）。
		// 注册表写入可能被安全软件拦截甚至长时间挂起，异步执行避免拖住托盘。
		go func() {
			if err := a.SetShellMenu(cfg.ShellMenuOn()); err != nil {
				log.Printf("同步资源管理器右键菜单失败: %v", err)
			}
		}()
	}

	log.Printf("启动 Wails 桌面窗口（WebView2）")
	werr := wailsui.Run(wailsui.RunOptions{
		Core:      a,
		Cfg:       cfg,
		CfgPath:   path,
		Bus:       bus,
		Smoke:     *smoke,
		LogWriter: logFile,
	})

	// 窗口退出后停止核心服务，并等它清理完（剪贴板监听、端口、KVM 注入线程）。
	stop()
	if cerr := a.Wait(); cerr != nil && werr == nil {
		werr = cerr
	}
	if *smoke > 0 {
		// 冒烟模式下 log 已被核心服务重定向到事件总线，这里把总线内容原样
		// 落到控制台/日志文件，方便 CI 判断"前端到底加载成功没有"。
		out := io.Writer(os.Stderr)
		if guiLogFile != nil {
			out = guiLogFile
		}
		for _, line := range bus.Logs() {
			fmt.Fprintf(out, "[smoke] %s\n", line)
		}
		fmt.Fprintf(out, "[smoke] wails 退出: err=%v\n", werr)
	}
	return werr
}
