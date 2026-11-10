package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"copywhere/internal/app"
	"copywhere/internal/config"
	"copywhere/internal/panel"
	"copywhere/internal/wailsui"
)

// cmdWails 是 copywhere 唯一的图形入口：同一进程内运行完整服务（发现/传输/
// 剪贴板/KVM）+ Wails v3 桌面窗口（WebView2）+ 系统托盘。
//
// 与 run（终端）共用命名互斥锁，避免两个实例抢剪贴板监听与端口。
func cmdWails(args []string) (err error) {
	fs := flag.NewFlagSet("wails", flag.ExitOnError)
	configPath := fs.String("config", "", "配置文件路径")
	smoke := fs.Duration("smoke", 0, "冒烟测试：窗口启动后等待该时长自动退出（如 5s）")
	fs.Parse(args)

	logWriter, closeLog := setupWailsLogging()
	defer closeLog()

	defer func() {
		if err != nil {
			log.Printf("桌面窗口启动/运行失败: %v", err)
		}
	}()

	if *smoke == 0 {
		if !acquireSingleton() {
			// 已有实例：把它的窗口找出来置前；找不到就明确报错，不再有
			// 浏览器窗口作为回退（旧 gui 方案已移除）。
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

	bus := panel.NewBus()
	a, err := app.Start(ctx, cfg, app.Options{GUI: true, Sink: bus, LogFile: logWriter})
	if err != nil {
		return err
	}

	log.Printf("启动 Wails 桌面窗口（WebView2）")
	werr := wailsui.Run(wailsui.RunOptions{
		Core:      a,
		Cfg:       cfg,
		CfgPath:   path,
		Bus:       bus,
		Smoke:     *smoke,
		LogWriter: logWriter,
	})

	// 窗口退出后停止核心服务，并等它清理完（剪贴板监听、端口、KVM 注入线程）。
	stop()
	if cerr := a.Wait(); cerr != nil && werr == nil {
		werr = cerr
	}
	if *smoke > 0 {
		// 冒烟模式下 log 已被核心服务重定向到事件总线，这里把总线内容原样
		// 落到控制台/日志文件，方便 CI 判断"前端到底加载成功没有"。
		for _, line := range bus.Logs() {
			fmt.Fprintf(logWriter, "[smoke] %s\n", line)
		}
		fmt.Fprintf(logWriter, "[smoke] wails 退出: err=%v\n", werr)
	}
	return werr
}

// setupWailsLogging 把日志同时写进 ~/.copywhere/wails.log 与终端（有终端时）。
// windowsgui 构建没有控制台，日志文件是"双击没反应"时唯一能查的痕迹。
// 超过 4MB 时滚动一份 wails.log.1，避免长期运行无限增长。
func setupWailsLogging() (io.Writer, func()) {
	home, err := os.UserHomeDir()
	if err != nil {
		return os.Stderr, func() {}
	}
	dir := filepath.Join(home, ".copywhere")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return os.Stderr, func() {}
	}
	logPath := filepath.Join(dir, "wails.log")
	if fi, err := os.Stat(logPath); err == nil && fi.Size() > 4<<20 {
		_ = os.Remove(logPath + ".1")
		_ = os.Rename(logPath, logPath+".1")
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return os.Stderr, func() {}
	}
	// 文件在前、终端在后：MultiWriter 在终端句柄不可用时会返回错误，
	// 但文件已经写入，不会丢日志。
	w := io.MultiWriter(f, os.Stderr)
	log.SetOutput(w)
	log.Printf("---- wails 模式启动 %s ----", time.Now().Format("2006-01-02 15:04:05"))
	return w, func() { f.Close() }
}
