// Package record 定义一次内容收发的记录视图，供核心服务与桌面前端共用。
package record

import "time"

// File 是一次内容收发记录（文件或文本）。
type File struct {
	Time   time.Time
	In     bool // true=接收，false=发送
	Peer   string
	Name   string // 文件名；文本为内容预览
	Size   int64
	Status string
	Detail string
	Path   string // 本机路径：接收=落盘路径，发送=源文件路径（文本/打包项可能为空）
	Text   bool   // true=文本同步记录（Name 为内容预览）
}
