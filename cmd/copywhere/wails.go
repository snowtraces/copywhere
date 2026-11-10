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
	"strings"
	"time"

	"copywhere/internal/app"
	"copywhere/internal/config"
	"copywhere/internal/wailsui"
	"copywhere/internal/webui"
)

// cmdWails 启动 Wails v3 桌面窗口版：同一进程内运行完整服务（发现/传输/
// 剪贴板/KVM）+ WebView2 窗口 + 系统托盘，不依赖浏览器。
//
// 与 gui 的关系：核心服务完全一致，只是承载 UI 的方式不同。为了避免
// 剪贴板监控与端口冲突，两者共用同一个命名互斥锁，不能同时启动。
func cmdWails(args []string) (err error) {
	fs := flag.NewFlagSet("wails", flag.ExitOnError)
	configPath := fs.String("config", "", "配置文件路径")
	smoke := fs.Duration("smoke", 0, "冒烟测试：窗口启动后等待该时长自动退出（如 5s）")
	fs.Parse(args)

	logWriter, closeLog := setupWailsLogging()
	defer closeLog()

	defer func() {
		if err != nil {
			log.Printf("wails 启动/运行失败: %v", err)
		}
	}()

	if *smoke == 0 {
		if !acquireSingleton() {
			// 已有实例在跑：优先把它的 Wails 窗口找出来置前；找不到（例如对方是
			// gui 浏览器模式）再尝试打开它的浏览器面板。
			if wailsui.ActivateExistingWindow() {
				log.Printf("已有 copywhere 实例在运行，已激活它的桌面窗口")
				return nil
			}
			if perr := openExistingPanel(*configPath); perr == nil {
				log.Printf("已有 copywhere 实例在运行，已打开它的浏览器面板")
				return nil
			}
			return fmt.Errorf("已有另一个 copywhere 实例在运行（gui / run / wails 不能同时启动）")
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

	bus := webui.NewBus()
	a, err := app.Start(ctx, cfg, app.Options{GUI: true, Sink: bus, LogFile: logWriter})
	if err != nil {
		return err
	}

	// Wails 模式下仍保留本地 HTTP 面板：既是排障入口，也让"第二个实例"
	// 的回落路径（打开已有面板）成立。面板只监听 127.0.0.1，需要访问令牌。
	if srv, serr := webui.NewServer(a, cfg, path, bus); serr != nil {
		log.Printf("本地 HTTP 面板启动失败（不影响桌面窗口）: %v", serr)
	} else {
		defer srv.Shutdown()
		srv.StartSnapshotLoop(ctx)
		urlFile := filepath.Join(filepath.Dir(path), "panel_url.txt")
		if werr := os.WriteFile(urlFile, []byte(srv.URL()), 0o600); werr != nil {
			log.Printf("记录面板地址失败: %v", werr)
		}
		defer os.Remove(urlFile)
		log.Printf("本地面板（浏览器回退）: %s", srv.URL())
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

// openExistingPanel 读取上一次运行写下的面板地址并用浏览器打开。
// 只用于"已有实例"的回落路径：地址里带访问令牌，文件权限 0600。
func openExistingPanel(cfgPath string) error {
	path := cfgPath
	if path == "" {
		path = config.DefaultPath()
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(path), "panel_url.txt"))
	if err != nil {
		return err
	}
	url := strings.TrimSpace(string(data))
	if url == "" {
		return fmt.Errorf("面板地址为空")
	}
	return webui.OpenBrowser(url)
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
