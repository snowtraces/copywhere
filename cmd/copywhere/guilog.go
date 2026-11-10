package main

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"copywhere/internal/app"
	"copywhere/internal/config"
)

// guiLogName 是 gui 模式的日志文件名（与配置文件同目录）。
const guiLogName = "gui.log"

// guiLogMaxBytes 超过该大小就清空重来，避免日志无限增长。
const guiLogMaxBytes = 4 << 20

// guiLogFile 是 gui 模式的日志文件句柄；app.Start 会把日志重定向到面板事件流，
// 因此还需把该句柄交给 app.Options.LogFile，让日志同时落盘。
var guiLogFile *os.File

// setupGUILogging 把日志同时落到 ~/.copywhere/gui.log。
//
// windowsgui 构建（双击启动、无控制台）时 stderr 是无效句柄，log 输出全部
// 丢失——用户遇到"双击没反应 / 托盘没出来 / 右键菜单没注册"时，既看不到
// 提示也没法反馈。这里在启动最开头就接上文件，出问题只要把这个文件发过来。
// 返回的函数用于关闭文件（不重置 log 输出，进程即将退出）。
func setupGUILogging() func() {
	dir := filepath.Dir(config.DefaultPath())
	// 目录只在 config.Save 时创建，而 config.Load 发生在 cmdGui 更后面：全新机器
	// 第一次双击时 ~/.copywhere 还不存在，不先建目录就会 OpenFile 失败，恰好丢掉
	// 最需要诊断的那一次启动（与 CHANGELOG"启动失败也要留痕"的承诺相反）。
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("创建日志目录 %s 失败（后续日志不可见）: %v", dir, err)
	}
	path := filepath.Join(dir, guiLogName)
	if st, err := os.Stat(path); err == nil && st.Size() > guiLogMaxBytes {
		// 只保留最近一次运行的日志，避免文件膨胀
		if err := os.Remove(path); err != nil {
			log.Printf("清理旧日志失败: %v", err)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		log.Printf("打开日志文件 %s 失败（后续日志不可见）: %v", path, err)
		return func() {}
	}
	// 顺序很关键：io.MultiWriter 遇到第一个写失败就中断，而 windowsgui 构建下
	// os.Stderr 是无效句柄、必然写失败。文件必须排在前面，否则日志永远为空。
	guiLogFile = f
	log.SetOutput(io.MultiWriter(f, os.Stderr))
	log.Printf("=== copywhere %s gui 启动 %s ===", app.Version, time.Now().Format("2006-01-02 15:04:05"))
	return func() {
		if err := f.Close(); err != nil {
			log.Printf("关闭日志文件失败: %v", err)
		}
	}
}
