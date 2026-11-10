// Package panel 是 copywhere 桌面前端（Wails 窗口）与运行核心之间的共享层：
// 事件总线 / 配置应用 / 记录视图 / 图标与 Shell 辅助。
//
// 历史背景：v0.4.0 的浏览器面板（internal/webui，HTTP + SSE + 内嵌 HTML）
// 已被 Wails v3 桌面窗口取代，本包只保留与 UI 载体无关、值得复用的部分。
package panel

import (
	"copywhere/internal/discovery"
	"copywhere/internal/trust"
)

// Core 是配置应用需要的核心服务能力（生产环境由 *app.App 实现）。
// 抽象成接口让 ApplyConfig 可以脱离真实服务做单元测试。
type Core interface {
	Peers() []discovery.Peer
	SelfName() string
	SelfIP() string
	SetPaused(bool)
	IsPaused() bool
	SendFiles(paths []string, total int64)
	SendText(text string)
	// 配对
	PairedWith(id string) bool
	RejectedWith(id string) bool
	PairedList() []trust.Paired
	PairWith(id string) error
	RespondPair(accept bool) error
	PendingPair() (id, name string, ok bool)
	Unpair(id string) error
	// KVM 布局：本机热更新 + 推送到对端（互为镜像）；
	// 携带改动前的布局，取消/换人时同步通知旧邻居解除
	SyncKVMFrom(oldLeft, oldRight string)
	// KVM 热启停：跨屏开关即时生效，无需重启
	SetKVMEnabled(on bool)
	// 触控板手势识别热启停（实验性），无需重启
	SetTouchpadGestures(on bool)
	// 触控板滚动输出倍率热更新（实验性，100=基准），无需重启
	SetTouchpadSpeed(pct int)
	// WatchMode 返回剪贴板监听方式："listener" | "viewer" | "none"（诊断展示）
	WatchMode() string
}
