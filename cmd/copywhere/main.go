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
	"strings"
	"time"

	"golang.org/x/term"

	"copywhere/internal/app"
	"copywhere/internal/clip"
	"copywhere/internal/config"
)

func usage() {
	fmt.Print(`copywhere — 局域网剪贴板/文件自动同步工具

用法:
  copywhere init                            生成默认配置（含随机 token）
  copywhere run [-config 路径] [-plain]     启动服务（节点发现 + 传输 + 剪贴板监控）
  copywhere nodes [-config 路径] [-wait 4s] 扫描并列出局域网在线节点
  copywhere send [-config 路径] [-text 内容] [-wait 4s] 文件/目录...
                                            立即发送到所有在线节点（不受大小阈值限制）
  copywhere clip-test                       本机剪贴板读写自检

工作方式:
  - 各节点通过 UDP 广播自动发现彼此，无需配置 IP；同一节点多网卡地址自动去重
  - run 运行期间，复制（Ctrl+C）小于阈值的文件或文本，会自动推送到所有在线节点
  - 对端收到文件后保存到接收目录并自动写回其剪贴板，直接 Ctrl+V 即可
  - run 默认启用三页终端界面（1 日志 / 2 在线节点 / 3 文件记录），
    -plain 或输出非终端时退化为纯日志
  - 所有节点必须使用相同 token 才能互通（编辑 config.json 中的 token）

配置文件: ~/.copywhere/config.json（接收目录: ~/.copywhere/files）
`)
}

func main() {
	log.SetFlags(log.Ltime)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "init":
		err = cmdInit(os.Args[2:])
	case "run":
		err = cmdRun(os.Args[2:])
	case "nodes":
		err = cmdNodes(os.Args[2:])
	case "send":
		err = cmdSend(os.Args[2:])
	case "clip-test":
		err = cmdClipTest(os.Args[2:])
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
	fmt.Printf("  token      : %s（各机器需改成一致）\n", cfg.Token)
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
