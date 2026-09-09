// Package app 组装配置、发现、传输与监控，并提供命令行入口使用的辅助函数。
package app

// Version 是程序版本号，构建时通过 -ldflags 注入：
//
//	go build -ldflags "-X copywhere/internal/app.Version=v0.1.0" ./cmd/copywhere
var Version = "dev"
