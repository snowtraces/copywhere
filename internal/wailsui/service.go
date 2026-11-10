// Package wailsui 是 copywhere 的 Wails v3 桌面前端（实验性）。
//
// 定位：copywhere 的唯一图形前端。核心服务（发现/传输/剪贴板/KVM）、
// 配置与事件总线全部复用；本包只提供：
//
//   - 一个 Wails v3 窗口（WebView2 承载全新前端，不经过浏览器/HTTP 窗口）；
//   - 绑定给前端的 Go 服务（传输时间线、设备坞、配对、KVM 布局、设置、日志）；
//   - 托盘菜单、关闭到托盘、文件拖放（拿真实路径直接投送）、窗口尺寸记忆。
//
// 与共享层 panel 的边界：
//   - 记录视图复用 panel.Bus / panel.FileView；
//   - 配置修改复用 panel.ApplyConfig（与窗口同一条校验与热更新链路）。
package wailsui

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	appcore "copywhere/internal/app"
	"copywhere/internal/config"
	"copywhere/internal/panel"
)

// maxImagePreview 是图片缩略图的上限：超过则回退成普通文件卡片，
// 避免把超大图片经 bindings（base64 后膨胀 4/3）塞进 WebView。
const maxImagePreview = 12 << 20

// State 是顶栏/设置页需要的运行状态快照。
type State struct {
	NodeName    string `json:"nodeName"`
	SelfIP      string `json:"selfIP"`
	Version     string `json:"version"`
	Paused      bool   `json:"paused"`
	ReceiveDir  string `json:"receiveDir"`
	ThresholdMB int64  `json:"thresholdMB"`
	AutoPaste   bool   `json:"autoPaste"`
	TextSync    bool   `json:"textSync"`
	PeerCount   int    `json:"peerCount"`
	PairedCount int    `json:"pairedCount"`
	KVMEnabled  bool   `json:"kvmEnabled"`
	KVMTouchpad bool   `json:"kvmTouchpad"`
	KVMLeft     string `json:"kvmLeft"`
	KVMRight    string `json:"kvmRight"`
	WatchMode   string `json:"watchMode"`
	ConfigPath  string `json:"configPath"`
	StartedAt   string `json:"startedAt"`
}

// Peer 是在线节点视图（附本机布局位置与配对状态）。
type Peer struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	IPs     []string `json:"ips"`
	TCPPort int      `json:"tcpPort"`
	Idle    string   `json:"idle"`
	Paired  bool     `json:"paired"`
	Blocked bool     `json:"blocked"`
	Side    string   `json:"side"` // "left" | "right" | ""
}

// PendingPair 是等待本机裁决的配对请求。
type PendingPair struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// PairedNode 是已配对设备（不含令牌）。
type PairedNode struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	PairedAt string `json:"pairedAt"`
}

// Settings 是设置页的完整可编辑视图。
type Settings struct {
	NodeName      string `json:"nodeName"`
	ReceiveDir    string `json:"receiveDir"`
	MaxAutoCopyMB int64  `json:"maxAutoCopyMB"`
	AutoPaste     bool   `json:"autoPaste"`
	TextSync      bool   `json:"textSync"`
	KVMEnabled    bool   `json:"kvmEnabled"`
	KVMTouchpad   bool   `json:"kvmTouchpad"`
	KVMTouchSpeed int    `json:"kvmTouchSpeed"`
	KVMMoveMs     int    `json:"kvmMoveMs"`
	KVMReflowMs   int    `json:"kvmReflowMs"`
	KVMSpeedPct   int    `json:"kvmSpeedPct"`
	KVMLeft       string `json:"kvmLeft"`
	KVMRight      string `json:"kvmRight"`
	DiscoveryPort int    `json:"discoveryPort"`
	TransferPort  int    `json:"transferPort"`
	KVMPort       int    `json:"kvmPort"`
	AnnounceSec   int    `json:"announceSec"`
	PeerTTLSec    int    `json:"peerTTLSec"`
	ConfigPath    string `json:"configPath"`
}

// Core 是桌面端需要的核心服务能力（生产环境由 *app.App 实现）。
// 抽成接口有两个目的：绑定服务可以单独测（见 service_test.go），
// 以及明确桌面端只依赖这些操作、不碰核心内部状态。
type Core interface {
	panel.Core
	// CopyText 写回本机剪贴板并标记为本机写入（避免二次广播）。
	CopyText(text string) error
}

// Service 是绑定给 Wails 前端的服务实例。
// Wails 以 goroutine 并发调用这些方法，所有对 cfg 的读写都在 mu 下进行。
type Service struct {
	core    Core
	cfg     *config.Config
	cfgPath string
	bus     *panel.Bus
	started time.Time

	mu sync.Mutex

	emitMu sync.RWMutex
	emit   func(name string, data any)

	hookMu       sync.RWMutex
	pauseChanged func(paused bool)
}

// NewService 创建绑定服务。core/cfg/bus 必须非 nil。
func NewService(core Core, cfg *config.Config, cfgPath string, bus *panel.Bus) *Service {
	if cfgPath == "" && cfg != nil {
		cfgPath = cfg.Path
	}
	return &Service{
		core:    core,
		cfg:     cfg,
		cfgPath: cfgPath,
		bus:     bus,
		started: time.Now(),
	}
}

// setEmitter 注入向 WebView 推送事件的方法（application.Event.Emit）。
func (s *Service) setEmitter(fn func(string, any)) {
	s.emitMu.Lock()
	s.emit = fn
	s.emitMu.Unlock()
}

// setPauseHook 注入"暂停状态被外部改变"的观察者（托盘复选框同步）。
func (s *Service) setPauseHook(fn func(bool)) {
	s.hookMu.Lock()
	s.pauseChanged = fn
	s.hookMu.Unlock()
}

func (s *Service) firePauseHook(paused bool) {
	s.hookMu.RLock()
	fn := s.pauseChanged
	s.hookMu.RUnlock()
	if fn != nil {
		fn(paused)
	}
}

// emitEvent 推送一条与共享事件总线同构的事件（type/data）。
func (s *Service) emitEvent(ev panel.Event) {
	s.emitMu.RLock()
	fn := s.emit
	s.emitMu.RUnlock()
	if fn == nil {
		return
	}
	fn("cw:event", ev)
}

// EmitSnapshot 立即推送节点表与状态快照（前端加载完成/操作完成后调用）。
func (s *Service) EmitSnapshot() {
	s.emitEvent(panel.Event{Type: panel.EvPeers, Data: s.Peers()})
	s.emitEvent(panel.Event{Type: panel.EvState, Data: s.State()})
}

// toast 让前端弹一条轻提示（拖放投送、失败原因等）。
func (s *Service) toast(msg string, isErr bool) {
	s.emitMu.RLock()
	fn := s.emit
	s.emitMu.RUnlock()
	if fn == nil {
		return
	}
	fn("cw:toast", map[string]any{"message": msg, "error": isErr})
}

// StartBridge 把共享事件总线（日志/记录/进度/配对请求）转成 Wails 事件，
// 并每 2 秒推送一次节点表与状态快照（camelCase 视图）。
func (s *Service) StartBridge(ctx context.Context) {
	ch, cancel := s.bus.Subscribe()
	defer cancel()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			s.emitEvent(ev)
		case <-ticker.C:
			s.EmitSnapshot()
		}
	}
}

// ---------- 只读视图 ----------

// State 返回运行状态快照。
func (s *Service) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	peers := s.core.Peers()
	return State{
		NodeName:    s.cfg.NodeName,
		SelfIP:      s.core.SelfIP(),
		Version:     appcore.Version,
		Paused:      s.core.IsPaused(),
		ReceiveDir:  s.cfg.ReceiveDir,
		ThresholdMB: s.cfg.MaxAutoCopyMB,
		AutoPaste:   s.cfg.AutoPaste,
		TextSync:    s.cfg.TextSync,
		PeerCount:   len(peers),
		PairedCount: len(s.core.PairedList()),
		KVMEnabled:  s.cfg.KVMOn(),
		KVMTouchpad: s.cfg.KVMTouchpadOn(),
		KVMLeft:     s.cfg.KVMLeft,
		KVMRight:    s.cfg.KVMRight,
		WatchMode:   s.core.WatchMode(),
		ConfigPath:  s.cfgPath,
		StartedAt:   s.started.Format(time.RFC3339),
	}
}

// Peers 返回在线节点视图（布局邻居带上 side）。
func (s *Service) Peers() []Peer {
	s.mu.Lock()
	left, right := s.cfg.KVMLeft, s.cfg.KVMRight
	s.mu.Unlock()

	peers := s.core.Peers()
	out := make([]Peer, 0, len(peers))
	for _, p := range peers {
		side := ""
		switch p.Name {
		case left:
			side = "left"
		case right:
			side = "right"
		}
		out = append(out, Peer{
			ID: p.ID, Name: p.Name, IPs: p.IPs, TCPPort: p.TCPPort,
			Idle:    time.Since(p.LastSeen).Round(time.Second).String(),
			Paired:  s.core.PairedWith(p.ID),
			Blocked: s.core.RejectedWith(p.ID),
			Side:    side,
		})
	}
	return out
}

// Records 返回时间线记录（旧 → 新，来自事件总线缓存）。
func (s *Service) Records() []panel.FileView { return s.bus.Files() }

// Logs 返回缓存的运行日志。
func (s *Service) Logs() []string { return s.bus.Logs() }

// Paired 返回已配对设备列表。
func (s *Service) Paired() []PairedNode {
	list := s.core.PairedList()
	out := make([]PairedNode, len(list))
	for i, e := range list {
		out[i] = PairedNode{ID: e.ID, Name: e.Name, PairedAt: e.PairedAt.Format("2006-01-02 15:04")}
	}
	return out
}

// Settings 返回设置页视图。
func (s *Service) Settings() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Settings{
		NodeName:      s.cfg.NodeName,
		ReceiveDir:    s.cfg.ReceiveDir,
		MaxAutoCopyMB: s.cfg.MaxAutoCopyMB,
		AutoPaste:     s.cfg.AutoPaste,
		TextSync:      s.cfg.TextSync,
		KVMEnabled:    s.cfg.KVMOn(),
		KVMTouchpad:   s.cfg.KVMTouchpadOn(),
		KVMTouchSpeed: s.cfg.KVMTouchpadSpeedPct(),
		KVMMoveMs:     s.cfg.KVMMoveMs,
		KVMReflowMs:   s.cfg.KVMReflowMs,
		KVMSpeedPct:   s.cfg.KVMSpeedPct,
		KVMLeft:       s.cfg.KVMLeft,
		KVMRight:      s.cfg.KVMRight,
		DiscoveryPort: s.cfg.DiscoveryPort,
		TransferPort:  s.cfg.TransferPort,
		KVMPort:       s.cfg.KVMPort, AnnounceSec: s.cfg.AnnounceSec,
		PeerTTLSec: s.cfg.PeerTTLSec,
		ConfigPath: s.cfgPath,
	}
}

// Info 是「关于」区块需要的静态信息。
type Info struct {
	Version    string `json:"version"`
	ConfigPath string `json:"configPath"`
	ReceiveDir string `json:"receiveDir"`
	ExePath    string `json:"exePath"`
	PanelMode  string `json:"panelMode"`
}

// Info 返回关于信息。
func (s *Service) Info() Info {
	exe, _ := os.Executable()
	s.mu.Lock()
	dir := s.cfg.ReceiveDir
	s.mu.Unlock()
	return Info{
		Version:    appcore.Version,
		ConfigPath: s.cfgPath,
		ReceiveDir: dir,
		ExePath:    exe,
		PanelMode:  "Wails v3（WebView2 独立窗口）",
	}
}

// ---------- 操作 ----------

// SetPaused 切换自动同步（手动发送不受影响）。
func (s *Service) SetPaused(paused bool) error {
	s.core.SetPaused(paused)
	s.firePauseHook(paused)
	s.EmitSnapshot()
	return nil
}

// SendText 手动发送文本到所有在线节点（不受阈值限制）。
func (s *Service) SendText(text string) error {
	if strings.TrimSpace(text) == "" {
		return errors.New("文本不能为空")
	}
	if len(s.core.Peers()) == 0 {
		return errors.New("未发现在线节点：请确认对端已运行 copywhere，且在同一网段")
	}
	go s.core.SendText(text)
	return nil
}

// SendFiles 把本机路径直接投送到所有在线节点（拖放/选择文件都走这里，
// 拿到的就是真实路径，无需先上传到临时目录）。
func (s *Service) SendFiles(paths []string) error {
	clean := make([]string, 0, len(paths))
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err != nil {
			log.Printf("忽略不可访问的拖入路径 %q: %v", p, err)
			continue
		}
		clean = append(clean, p)
	}
	if len(clean) == 0 {
		return errors.New("没有可发送的文件（路径不存在或为空）")
	}
	if n, reason := s.sendablePeers(); n == 0 {
		return errors.New(reason)
	}
	go s.core.SendFiles(clean, 0)
	return nil
}

// sendablePeers 统计可直接投递的在线节点（已配对且未被鉴权拒绝）。
func (s *Service) sendablePeers() (int, string) {
	peers := s.core.Peers()
	if len(peers) == 0 {
		return 0, "未发现在线节点：请确认对端已运行 copywhere，且两台机器在同一网段"
	}
	n := 0
	for _, p := range peers {
		if s.core.PairedWith(p.ID) && !s.core.RejectedWith(p.ID) {
			n++
		}
	}
	if n == 0 {
		return 0, "在线节点均未配对（或鉴权被拒）：请先在设备坞完成配对"
	}
	return n, ""
}

// SaveSettings 应用设置页提交的字段（与网页窗口 /api/config 同一实现）。
// in 使用窗口的字段名（node_name、kvm_left 等），只提交改动过的字段即可。
func (s *Service) SaveSettings(in map[string]any) error {
	if len(in) == 0 {
		return errors.New("没有需要保存的修改")
	}
	s.mu.Lock()
	err := panel.ApplyConfig(s.core, s.cfg, s.cfgPath, in)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	s.EmitSnapshot()
	return nil
}

// SetPausedFromTray 供托盘菜单调用：切换后同步托盘复选框与窗口 UI。
func (s *Service) SetPausedFromTray(paused bool) {
	s.core.SetPaused(paused)
	s.firePauseHook(paused)
	s.EmitSnapshot()
}

// SetKVMNeighbor 配置某一侧的邻居节点：side 为 "left"/"right"，name 为空表示清除。
func (s *Service) SetKVMNeighbor(side, name string) error {
	key := ""
	switch side {
	case "left":
		key = "kvm_left"
	case "right":
		key = "kvm_right"
	default:
		return fmt.Errorf("未知的布局位置: %q", side)
	}
	return s.SaveSettings(map[string]any{key: strings.TrimSpace(name)})
}

// Pair 主动向指定节点发起配对（同步等待对端裁决）。
func (s *Service) Pair(id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("缺少节点 id")
	}
	if err := s.core.PairWith(id); err != nil {
		return fmt.Errorf("配对失败: %w", err)
	}
	s.EmitSnapshot()
	return nil
}

// Unpair 解除与指定节点的配对。
func (s *Service) Unpair(id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("缺少节点 id")
	}
	if err := s.core.Unpair(id); err != nil {
		return fmt.Errorf("解除配对失败: %w", err)
	}
	s.EmitSnapshot()
	return nil
}

// PendingPair 返回待裁决的配对请求；没有时返回 nil。
func (s *Service) PendingPair() *PendingPair {
	id, name, ok := s.core.PendingPair()
	if !ok {
		return nil
	}
	return &PendingPair{ID: id, Name: name}
}

// RespondPair 对配对请求做出裁决。
func (s *Service) RespondPair(accept bool) error {
	if err := s.core.RespondPair(accept); err != nil {
		return fmt.Errorf("处理配对请求失败: %w", err)
	}
	s.EmitSnapshot()
	return nil
}

// OpenPath 用资源管理器打开路径；selectInFolder 为 true 时高选中该文件。
func (s *Service) OpenPath(path string, selectInFolder bool) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("路径为空")
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("路径不存在: %s", path)
	}
	if err := panel.RevealInExplorer(path, selectInFolder); err != nil {
		return fmt.Errorf("打开失败: %w", err)
	}
	return nil
}

// OpenReceiveDir 在资源管理器中打开接收目录。
func (s *Service) OpenReceiveDir() error {
	s.mu.Lock()
	dir := s.cfg.ReceiveDir
	s.mu.Unlock()
	if dir == "" {
		return errors.New("接收目录未配置")
	}
	if _, err := os.Stat(dir); err != nil {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("创建接收目录失败: %w", err)
		}
	}
	if err := panel.RevealInExplorer(dir, false); err != nil {
		return fmt.Errorf("打开接收目录失败: %w", err)
	}
	return nil
}

// CopyText 把文本写回本机剪贴板（不触发二次广播）。
func (s *Service) CopyText(text string) error {
	if strings.TrimSpace(text) == "" {
		return errors.New("内容为空")
	}
	if err := s.core.CopyText(text); err != nil {
		return fmt.Errorf("写入剪贴板失败: %w", err)
	}
	return nil
}

// ImagePreview 返回接收目录内图片的 data URL（缩略图/灯箱用）。
// 安全边界与窗口 /api/file 一致：只允许接收目录内的常见图片扩展名。
func (s *Service) ImagePreview(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("路径为空")
	}
	ct, ok := imageContentType(path)
	if !ok {
		return "", errors.New("仅支持图片预览")
	}
	s.mu.Lock()
	root := s.cfg.ReceiveDir
	s.mu.Unlock()
	absRoot, err := filepath.Abs(root)
	if err != nil || absRoot == "" {
		return "", errors.New("接收目录不可用")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", errors.New("路径非法")
	}
	rel, err := filepath.Rel(absRoot, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("仅允许预览接收目录内的文件")
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return "", errors.New("文件不存在")
	}
	if fi.Size() > maxImagePreview {
		return "", fmt.Errorf("图片超过 %d MB，已跳过预览", maxImagePreview>>20)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", fmt.Errorf("读取图片失败: %w", err)
	}
	return "data:" + ct + ";base64," + base64Encode(data), nil
}

// UiReady 是前端加载完成后的回执：只写日志，用于确认 WebView/运行时就绪
// （双击启动没有控制台，这一行是排障时"前端到底加载了没有"的第一手证据）。
func (s *Service) UiReady(detail string) string {
	log.Printf("Wails 前端已加载：%s", detail)
	return appcore.Version
}
