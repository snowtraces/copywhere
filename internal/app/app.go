// Package app 组装配置、发现、传输与监控，并提供命令行入口使用的辅助函数。
package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/windows/registry"

	"copywhere/internal/bytesize"
	"copywhere/internal/clip"
	"copywhere/internal/config"
	"copywhere/internal/discovery"
	"copywhere/internal/input"
	"copywhere/internal/kvm"
	"copywhere/internal/monitor"
	"copywhere/internal/transport"
	"copywhere/internal/trust"
	"copywhere/internal/ui"
)

const pendingTTL = 60 * time.Second

// Options 控制 Run 的运行形态。
type Options struct {
	// Interactive 为 true 时启用 TUI（日志/在线节点/文件记录三个标签页）；
	// 为 false 时纯日志输出到标准输出。
	Interactive bool
	// GUI 为 true 时服务在后台运行，由调用方（gui 命令）提供托盘与 Web 面板；
	// 此时日志与事件写入 Sink，不占用标准输出。
	GUI bool
	// Sink 非 nil 时接收结构化事件（webui.Bus 实现），供 GUI 面板实时展示。
	Sink EventSink
}

// Progress 描述一次文件发送的实时进度。
type Progress struct {
	Peer  string `json:"peer"`
	Name  string `json:"name"`
	Sent  int64  `json:"sent"`
	Total int64  `json:"total"`
}

// EventSink 接收运行事件（日志行、文件记录、发送进度、配对请求）。
// TUI 模式下为 nil；GUI 模式下由 webui.Bus 实现。
type EventSink interface {
	Log(line string)
	AddFile(r ui.FileRecord)
	Progress(p Progress)
	PairRequest(id, name string)
}

// sinkWriter 把 log 包的整行输出转成 Log 事件。
type sinkWriter struct {
	sink EventSink
	buf  []byte
}

func (w *sinkWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(string(w.buf[:i]), "\r")
		w.buf = w.buf[i+1:]
		if line != "" {
			w.sink.Log(line)
		}
	}
	return len(p), nil
}

type pendingEntry struct {
	kind    string // "files" | "text"
	paths   []string
	text    string
	expires time.Time
}

// SendResult 是一次对端发送的结果。
type SendResult struct {
	ID   string // 对端节点指纹
	Peer string
	Name string
	Size int64
	OK   bool
	Msg  string
}

// App 是运行中的 copywhere 实例。
type App struct {
	cfg      *config.Config
	store    *discovery.Store
	selfID   string
	ui       *ui.Controller
	sink     EventSink
	trust    *trust.Store
	ownSeq   atomic.Uint32
	lastRecv atomic.Int64
	paused   atomic.Bool // 自动同步暂停开关（手动 send 不受影响）

	opts   Options
	ctx    context.Context
	cancel context.CancelFunc
	kvmSvc *kvm.Service // 已启动的 KVM 服务（布局可热更新）

	pendingMu sync.Mutex
	pending   []pendingEntry

	rejectedMu sync.Mutex
	rejected   map[string]bool // 因 token 不一致被暂停发送的节点（按指纹）

	pairMu  sync.Mutex
	pairReq *pairRequest // 待本机用户裁决的配对请求
}

// pairRequest 是一次待裁决的配对请求。
type pairRequest struct {
	offer   transport.PairOffer
	reply   chan transport.PairDecision // 缓冲 1，RespondPair 投递裁决
	expires time.Time
}

func newApp(cfg *config.Config) (*App, error) {
	store := discovery.NewStore(nodeID())
	store.SetLocalSubnets(localSubnets())
	a := &App{cfg: cfg, store: store, selfID: nodeID(), rejected: map[string]bool{}}
	store.SetRestartCallback(a.onPeerRestart)
	// 信任库与配置同目录（peers.json）；读取失败不阻断启动，仅退化为空库
	cfgPath := cfg.Path
	if cfgPath == "" {
		cfgPath = config.DefaultPath()
	}
	trPath := filepath.Join(filepath.Dir(cfgPath), "peers.json")
	tr, err := trust.Load(trPath)
	if err != nil {
		log.Printf("读取配对信任库失败（按空库处理）: %v", err)
		tr = trust.New(trPath)
	}
	a.trust = tr
	return a, nil
}

// onPeerRestart 同一指纹的节点重启（可能已修正 token）时解除暂停发送。
func (a *App) onPeerRestart(id, name string) {
	a.rejectedMu.Lock()
	defer a.rejectedMu.Unlock()
	if a.rejected[id] {
		delete(a.rejected, id)
		log.Printf("节点 %q 已重启，恢复向其发送", name)
	}
}

// markRejected 因鉴权被拒暂停向该节点发送；只提示一次。
func (a *App) markRejected(p discovery.Peer) {
	a.rejectedMu.Lock()
	defer a.rejectedMu.Unlock()
	if a.rejected[p.ID] {
		return
	}
	a.rejected[p.ID] = true
	if a.trust.Has(p.ID) {
		log.Printf("节点 %q 拒绝传输（配对可能已被对端解除），已暂停向其发送；可在面板重新配对", p.Name)
	} else {
		log.Printf("节点 %q 拒绝传输（未配对且 token 不一致），已暂停向其发送；可在面板配对", p.Name)
	}
}

func (a *App) isRejected(id string) bool {
	a.rejectedMu.Lock()
	defer a.rejectedMu.Unlock()
	return a.rejected[id]
}

// sendablePeers 返回在线且未因 token 不一致被暂停的节点。
func (a *App) sendablePeers() []discovery.Peer {
	var out []discovery.Peer
	for _, p := range a.store.Alive(a.ttl()) {
		if !a.isRejected(p.ID) {
			out = append(out, p)
		}
	}
	return out
}

// nodeID 是本机的稳定节点指纹：同一台机器重启后不变，
// 对端据此把机器的全部来源地址合并到同一节点，而不会误判为新节点。
var nodeID = sync.OnceValue(func() string {
	if guid := machineGUID(); guid != "" {
		return shortHash("guid:" + guid)
	}
	// 退回方案：主机名 + 网卡 MAC
	host, _ := os.Hostname()
	macs := ""
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		macs += ifc.HardwareAddr.String() + ","
	}
	return shortHash(fmt.Sprintf("host:%s|%s", host, macs))
})

// machineGUID 读取 Windows 安装时生成的 MachineGuid（重装系统才会变化）。
func machineGUID() string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Cryptography`, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	v, _, err := k.GetStringValue("MachineGuid")
	if err != nil {
		return ""
	}
	return v
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

// precheckPorts 在启动服务前预检端口占用，尽早失败并给出明确原因，
// 避免“传输/发现静默失效、只剩剪贴板监控”的半残状态。
func precheckPorts(cfg *config.Config) error {
	if ln, err := net.Listen("tcp4", fmt.Sprintf("0.0.0.0:%d", cfg.TransferPort)); err != nil {
		return fmt.Errorf("传输端口 %d/tcp 被占用: %w（是否已有另一个 copywhere 实例在运行？）",
			cfg.TransferPort, err)
	} else {
		ln.Close()
	}
	if c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: cfg.DiscoveryPort}); err != nil {
		return fmt.Errorf("发现端口 %d/udp 被占用: %w（是否已有另一个 copywhere 实例在运行？）",
			cfg.DiscoveryPort, err)
	} else {
		c.Close()
	}
	return nil
}

// Run 启动完整服务：发现 + 传输 + 剪贴板监控（+ 可选 TUI），阻塞直到退出。
func Run(ctx context.Context, cfg *config.Config, opts Options) error {
	a, err := Start(ctx, cfg, opts)
	if err != nil {
		return err
	}
	return a.Wait()
}

// Start 启动完整服务并立即返回运行中的 App；调用方随后用 Wait 阻塞等待退出，
// 或直接通过返回的 App 操作（GUI 模式下由 webui 面板调用其导出方法）。
func Start(ctx context.Context, cfg *config.Config, opts Options) (*App, error) {
	if err := precheckPorts(cfg); err != nil {
		return nil, err
	}
	a, err := newApp(cfg)
	if err != nil {
		return nil, err
	}
	a.opts = opts
	// 注意：cancel 不能 defer 在 Start 里——Start 会立即返回，
	// 提前取消会让传输/发现/TUI 全部秒退（表现为无法握手、TUI 无法启动）。
	// 生命周期移交给 App，由 Wait 结束时释放。
	runCtx, cancel := context.WithCancel(ctx)
	a.cancel = cancel
	a.ctx = runCtx

	switch {
	case opts.Interactive:
		a.ui = ui.NewController(func() []discovery.Peer { return a.store.Alive(a.ttl()) },
			a.ttl(), cfg.NodeName, selfIP())
		log.SetOutput(a.ui.Hub) // 日志进入 TUI 的日志页
	case opts.Sink != nil:
		a.sink = opts.Sink
		log.SetOutput(&sinkWriter{sink: a.sink}) // 日志进入 GUI 面板事件流
	default:
		log.SetOutput(ui.NewHub(os.Stdout)) // 纯日志模式，镜像到标准输出
	}

	go func() {
		if err := transport.Server(runCtx, cfg, transport.Handlers{
			OnFile:    a.onFileReceived,
			OnText:    a.onTextReceived,
			Authorize: a.authorize,
			OnPair:    a.onPairRequest,
			OnKVM:     a.onKVMSync,
		}); err != nil {
			log.Printf("传输服务异常退出: %v", err)
		}
	}()
	go func() {
		interval := time.Duration(cfg.AnnounceSec) * time.Second
		if err := discovery.Run(runCtx, cfg.DiscoveryPort, cfg.TransferPort, interval, cfg.NodeName, a.selfID, a.store); err != nil {
			log.Printf("节点发现异常退出: %v", err)
		}
	}()
	go a.flushLoop(runCtx)

	printBanner(cfg)
	var kvmSvc *kvm.Service
	if cfg.KVMOn() {
		kvmSvc = kvm.NewService(
			kvm.Config{
				Port:         cfg.KVMPort,
				Left:         cfg.KVMLeft,
				Right:        cfg.KVMRight,
				EntryMonitor: cfg.KVMEntryMonitorIdx(),
				SelfID:       a.selfID,
				MoveInterval: cfg.KVMMoveInterval(),
				ReflowStep:   cfg.KVMReflowStep(),
				SpeedPercent: cfg.KVMSpeedFactor(),
				TokenForPeer: func(id string) (string, bool) {
					e, ok := a.trust.Get(id)
					return e.PeerToken, ok && e.PeerToken != ""
				},
				PairTokenFor: func(id string) (string, bool) {
					e, ok := a.trust.Get(id)
					return e.PairToken, ok && e.PairToken != ""
				},
			},
			cfg.NodeName, a.store, a.ttl(), input.DefaultInjector{})
		a.kvmSvc = kvmSvc
		if err := kvmSvc.Start(runCtx); err != nil {
			log.Printf("KVM 服务启动失败: %v", err)
		} else {
			if err := input.Start(input.Callbacks{
				OnMouseMove:     kvmSvc.OnMouseMove,
				OnMouseButton:   kvmSvc.OnMouseButton,
				OnWheel:         kvmSvc.OnWheel,
				OnKey:           kvmSvc.OnKey,
				OnLocalActivity: kvmSvc.OnLocalActivity,
			}); err != nil {
				log.Printf("输入钩子启动失败: %v", err)
			}
			log.Printf("KVM 跨屏已启用：左邻 %q，右邻 %q，端口 %d/tcp（Ctrl+Alt+Shift+X 紧急退出）",
				cfg.KVMLeft, cfg.KVMRight, cfg.KVMPort)
		}
	}
	if opts.Interactive || opts.GUI {
		go monitor.Run(runCtx, monitor.Guard{OwnSeq: &a.ownSeq, LastReceivedAt: &a.lastRecv, Paused: &a.paused},
			func() int64 { return cfg.Threshold() }, func() bool { return cfg.TextSync }, a)
	}
	return a, nil
}

// Wait 阻塞直到服务退出（TUI 退出 / ctx 结束 / 纯日志模式下监控循环结束）。
// 无论从哪条路径返回，都会释放 Start 建立的子 context。
func (a *App) Wait() error {
	defer a.cancel()
	if a.ui != nil {
		// TUI 退出（q/Esc/Ctrl+C）即整体退出
		return a.ui.Run(a.ctx)
	}
	if a.opts.GUI {
		// GUI 模式：监控已在后台运行，等待 ctx（托盘退出 / Ctrl+C）
		<-a.ctx.Done()
		return nil
	}
	monitor.Run(a.ctx, monitor.Guard{OwnSeq: &a.ownSeq, LastReceivedAt: &a.lastRecv, Paused: &a.paused},
		func() int64 { return a.cfg.Threshold() }, func() bool { return a.cfg.TextSync }, a)
	return nil
}

// ---------- GUI 面板接口 ----------

// Peers 返回当前在线节点快照。
func (a *App) Peers() []discovery.Peer { return a.store.Alive(a.ttl()) }

// SelfName 返回本机节点显示名。
func (a *App) SelfName() string { return a.cfg.NodeName }

// SelfIP 返回本机展示用的首选地址。
func (a *App) SelfIP() string { return selfIP() }

// SetPaused 暂停/恢复自动同步；手动发送不受影响。
func (a *App) SetPaused(p bool) {
	if a.paused.Swap(p) == p {
		return
	}
	if p {
		log.Printf("自动同步已暂停（手动发送不受影响）")
	} else {
		log.Printf("自动同步已恢复")
	}
}

// IsPaused 返回自动同步是否处于暂停状态。
func (a *App) IsPaused() bool { return a.paused.Load() }

// RejectedWith 返回节点是否因鉴权被拒处于暂停发送状态。
func (a *App) RejectedWith(id string) bool {
	a.rejectedMu.Lock()
	defer a.rejectedMu.Unlock()
	return a.rejected[id]
}

// ---------- 配对 ----------

const pairPendingTTL = 150 * time.Second

// PairedWith 返回节点是否已与本机配对。
func (a *App) PairedWith(id string) bool { return a.trust.Has(id) }

// PairedList 返回全部配对记录。
func (a *App) PairedList() []trust.Paired { return a.trust.List() }

// Unpair 解除与指定节点的配对：删除独立令牌，双方此后均无法再以原令牌传输。
func (a *App) Unpair(id string) error {
	e, ok := a.trust.Get(id)
	if !ok {
		return fmt.Errorf("该节点未配对")
	}
	if err := a.trust.Remove(id); err != nil {
		return err
	}
	a.rejectedMu.Lock()
	delete(a.rejected, id)
	a.rejectedMu.Unlock()
	log.Printf("已解除与节点 %q 的配对", e.Name)
	return nil
}

// PairWith 主动向指定节点发起配对：发送本机身份与为本机新签发的配对令牌，
// 对端接受后沿同一连接返回对端签发的令牌，双方各存一条记录。
func (a *App) PairWith(id string) error {
	var peer discovery.Peer
	found := false
	for _, p := range a.store.Alive(a.ttl()) {
		if p.ID == id {
			peer, found = p, true
			break
		}
	}
	if !found {
		return fmt.Errorf("节点不在线或未发现（配对前需双方运行 copywhere）")
	}
	if id == a.selfID {
		return fmt.Errorf("不能与本机配对")
	}
	// 为对端新签发一枚配对令牌（不复用、不改动 config token）
	issueToken, err := config.RandomHex(16)
	if err != nil {
		return fmt.Errorf("生成配对令牌失败: %w", err)
	}
	offer, _ := json.Marshal(transport.PairOffer{
		ID: a.selfID, Name: a.cfg.NodeName, Token: issueToken,
	})
	data := offer
	var lastErr error
	payload := func(w io.Writer) error { _, err := w.Write(data); return err }
	for _, ip := range peer.IPs {
		hdr := transport.Header{
			V: 1, Type: "pair", Name: a.cfg.NodeName, Sender: a.cfg.NodeName,
			SenderID: a.selfID, Size: int64(len(data)),
		}
		log.Printf("向节点 %q @ %s 发起配对请求", peer.Name, ip)
		resp, err := transport.Send(ip, peer.TCPPort, hdr, payload, 3*time.Minute+15*time.Second)
		if err == nil && resp.OK {
			if err := a.trust.Add(id, resp.PeerName, issueToken, resp.Token); err != nil {
				return fmt.Errorf("保存配对记录失败: %w", err)
			}
			a.rejectedMu.Lock()
			delete(a.rejected, id)
			a.rejectedMu.Unlock()
			log.Printf("与节点 %q 配对成功（指纹 %s…）", resp.PeerName, shortHash(id))
			return nil
		}
		if err == nil && !resp.OK {
			lastErr = fmt.Errorf("对方拒绝了配对")
			break
		}
		if strings.Contains(err.Error(), "pair rejected") {
			lastErr = fmt.Errorf("对方拒绝了配对")
			break
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("节点没有可用地址")
	}
	return fmt.Errorf("与节点 %q 配对失败: %w", peer.Name, lastErr)
}

// onPairRequest 是传输层的配对回调：登记待裁决请求并通知面板，
// 然后阻塞等待本机用户在面板中接受/拒绝（或超时自动拒绝）。
func (a *App) onPairRequest(offer transport.PairOffer) transport.PairDecision {
	if offer.ID == "" || offer.ID == a.selfID || offer.Token == "" {
		return transport.PairDecision{Accepted: false}
	}
	ch := make(chan transport.PairDecision, 1)
	req := &pairRequest{offer: offer, reply: ch, expires: time.Now().Add(pairPendingTTL)}

	a.pairMu.Lock()
	if a.pairReq != nil { // 已有待裁决请求：让旧的立即失效，只保留最新
		a.pairReq.reply <- transport.PairDecision{Accepted: false}
	}
	a.pairReq = req
	a.pairMu.Unlock()

	log.Printf("节点 %q（指纹 %s…）请求配对，等待本机确认（%s 内有效）",
		offer.Name, shortHash(offer.ID), pairPendingTTL)
	if a.sink != nil {
		a.sink.PairRequest(offer.ID, offer.Name)
	}

	select {
	case d := <-ch:
		return d
	case <-time.After(pairPendingTTL + 5*time.Second):
		a.pairMu.Lock()
		if a.pairReq == req {
			a.pairReq = nil
		}
		a.pairMu.Unlock()
		return transport.PairDecision{Accepted: false}
	}
}

// RespondPair 裁决当前待处理的配对请求。
// 接受时为本机新签发一枚配对令牌，连同发起方出示的令牌一起入库。
func (a *App) RespondPair(accept bool) error {
	a.pairMu.Lock()
	req := a.pairReq
	a.pairReq = nil
	a.pairMu.Unlock()
	if req == nil || time.Now().After(req.expires) {
		return fmt.Errorf("没有待处理的配对请求")
	}
	if !accept {
		req.reply <- transport.PairDecision{Accepted: false}
		return nil
	}
	issueToken, err := config.RandomHex(16)
	if err != nil {
		req.reply <- transport.PairDecision{Accepted: false}
		return fmt.Errorf("生成配对令牌失败: %w", err)
	}
	// 入库：PairToken = 本机新签发给发起方的令牌（其后续发送须携带）；
	// PeerToken = 发起方出示的令牌（本机后续向其发送时携带）
	if err := a.trust.Add(req.offer.ID, req.offer.Name, issueToken, req.offer.Token); err != nil {
		req.reply <- transport.PairDecision{Accepted: false}
		return fmt.Errorf("保存配对记录失败: %w", err)
	}
	req.reply <- transport.PairDecision{
		Accepted: true, Token: issueToken, ID: a.selfID, Name: a.cfg.NodeName,
	}
	return nil
}

// PendingPair 返回当前待裁决的配对请求（面板刷新/重开时恢复弹窗用）。
func (a *App) PendingPair() (id, name string, ok bool) {
	a.pairMu.Lock()
	defer a.pairMu.Unlock()
	if a.pairReq == nil || time.Now().After(a.pairReq.expires) {
		return "", "", false
	}
	return a.pairReq.offer.ID, a.pairReq.offer.Name, true
}

// ---------- KVM 布局同步 ----------

// SyncKVM 把当前布局落地并推送：本机 KVM 服务热更新邻居；
// 同时告知每个在线且已配对的邻居"把我放到你的另一侧"，实现多机自动同步。
// （A 是 B 的左邻 ⟺ B 是 A 的右邻，两端只需在一端配置。）
func (a *App) SyncKVM() {
	if a.kvmSvc != nil {
		a.kvmSvc.UpdateNeighbors(a.cfg.KVMLeft, a.cfg.KVMRight)
		a.kvmSvc.UpdateTunables(a.cfg.KVMMoveInterval(), a.cfg.KVMReflowStep(), a.cfg.KVMSpeedFactor())
	}
	a.syncKVMOne(a.cfg.KVMLeft, "right")
	a.syncKVMOne(a.cfg.KVMRight, "left")
}

// syncKVMOne 把"本机应作为对方的 remoteSide 邻居"推送给名为 neighbor 的节点。
func (a *App) syncKVMOne(neighbor, remoteSide string) {
	if neighbor == "" {
		return
	}
	for _, p := range a.store.Alive(a.ttl()) {
		if p.Name != neighbor {
			continue
		}
		if !a.trust.Has(p.ID) {
			log.Printf("KVM 布局同步跳过 %q：尚未配对（同步依赖配对通道）", p.Name)
			return
		}
		payload, err := json.Marshal(transport.KVMSync{
			PeerID: a.selfID, PeerName: a.cfg.NodeName, Side: remoteSide,
		})
		if err != nil {
			return
		}
		hdr := transport.Header{
			V: 1, Type: "kvm", Name: "kvm-sync", Sender: a.cfg.NodeName,
			Size: int64(len(payload)),
		}
		pp, hh, pl := p, hdr, payload
		go func() {
			makePayload := func() (func(io.Writer) error, error) {
				return func(w io.Writer) error { _, err := w.Write(pl); return err }, nil
			}
			if _, err := a.sendToPeer(pp, hh, makePayload, 15*time.Second); err != nil {
				log.Printf("KVM 布局同步到 %q 失败: %v", pp.Name, err)
			} else {
				log.Printf("KVM 布局已同步到 %q（本机为其%s邻）", pp.Name,
					map[string]string{"left": "右", "right": "左"}[remoteSide])
			}
		}()
		return
	}
	log.Printf("KVM 布局同步跳过 %q：当前不在线（对端上线后在面板重新保存布局即可同步）", neighbor)
}

// onKVMSync 处理邻居推来的布局同步：把对方登记为指定侧邻居，
// 落盘并热更新本机 KVM 服务（无需重启）。
func (a *App) onKVMSync(kv transport.KVMSync) error {
	if kv.Side != "left" && kv.Side != "right" {
		return fmt.Errorf("无效的侧别 %q", kv.Side)
	}
	if kv.PeerID == a.selfID {
		return fmt.Errorf("不能与本机配对布局")
	}
	if kv.Side == "left" {
		a.cfg.KVMLeft = kv.PeerName
	} else {
		a.cfg.KVMRight = kv.PeerName
	}
	if err := a.cfg.Save(a.cfgPath()); err != nil {
		return fmt.Errorf("保存配置失败: %w", err)
	}
	if a.kvmSvc != nil {
		a.kvmSvc.UpdateNeighbors(a.cfg.KVMLeft, a.cfg.KVMRight)
	}
	log.Printf("布局同步：已将节点 %q 设为本机%s邻并即时生效", kv.PeerName,
		map[string]string{"left": "左", "right": "右"}[kv.Side])
	return nil
}

// cfgPath 返回配置文件路径（cfg.Path 由 Load 填充，测试或手工构造时兜底默认路径）。
func (a *App) cfgPath() string {
	if a.cfg.Path != "" {
		return a.cfg.Path
	}
	return config.DefaultPath()
}

// addFile 把文件记录投递到当前前端（TUI 或 GUI 事件流）。
func (a *App) addFile(r ui.FileRecord) {
	if a.ui != nil {
		a.ui.AddFile(r)
	} else if a.sink != nil {
		a.sink.AddFile(r)
	}
}

// ---------- 接收回调 ----------

func (a *App) onFileReceived(sender, path string, dup bool, bundle bool) {
	a.lastRecv.Store(time.Now().UnixNano())

	// 发送端因多文件/目录自动打包的合成 zip：解包到本次接收子目录，
	// 剪贴板回贴解包后的内容（单个目录或多个文件）。用户亲手发送的 zip
	//（bundle=false）原样保留，不误解包。
	targets := []string{path} // 写回剪贴板的路径列表（默认为文件本身）
	unpacked := false
	if bundle && strings.EqualFold(filepath.Ext(path), ".zip") {
		tops, err := unpackZip(path)
		if err != nil {
			log.Printf("自动解包失败（保留原 zip）: %v", err)
		} else {
			targets = tops
			unpacked = true
			log.Printf("已自动解包为 %d 项内容", len(tops))
			// 后续日志/统计/记录全部指向解出的内容而非已删除的 zip：
			// 单个顶层目标（文件夹包）用目标本身，多目标指向接收子目录
			path = bundleRecordPath(tops)
		}
	}

	if a.cfg.AutoPaste {
		if err := clip.SetFiles(targets); err != nil {
			log.Printf("写入剪贴板失败: %v", err)
		} else {
			a.ownSeq.Store(clip.Seq())
		}
	}
	note := ""
	if unpacked {
		note = "（已自动解包）"
	} else if dup {
		note = "（已存在相同内容）"
	}
	log.Printf("已接收文件 %s（来自 %s）%s，可直接 Ctrl+V", path, sender, note)
	if a.ui != nil || a.sink != nil {
		size := int64(0)
		if fi, err := os.Stat(path); err == nil {
			size = fi.Size()
		}
		status := "已接收"
		if unpacked {
			status = "已接收（已解包）"
		} else if dup {
			status = "重复内容"
		}
		recordName := filepath.Base(path)
		if unpacked && len(targets) > 1 {
			recordName += fmt.Sprintf("（%d 项）", len(targets))
		}
		a.addFile(ui.FileRecord{
			Time: time.Now(), In: true, Peer: sender,
			Name: recordName, Size: size, Status: status, Detail: path,
		})
	}
}

// bundleRecordPath 返回解包后记录应指向的路径：
// 单个顶层目标（文件夹包）即目标本身；多个目标则指向本次接收的子目录，
// 便于一键定位全部内容。
func bundleRecordPath(targets []string) string {
	if len(targets) == 1 {
		return targets[0]
	}
	if len(targets) == 0 {
		return ""
	}
	return filepath.Dir(targets[0])
}

func (a *App) onTextReceived(sender, text string) {
	a.lastRecv.Store(time.Now().UnixNano())
	if a.cfg.TextSync {
		if err := clip.SetText(text); err != nil {
			log.Printf("写入剪贴板失败: %v", err)
		} else {
			a.ownSeq.Store(clip.Seq())
		}
	}
	preview := text
	if len(preview) > 40 {
		preview = preview[:40] + "…"
	}
	log.Printf("已接收文本（来自 %s）：%q，可直接 Ctrl+V", sender, preview)
}

// ---------- 发送 ----------

// SendFiles 同步一批剪贴板文件（多文件/目录自动打包 zip）；对端不可达时进入重试队列。
func (a *App) SendFiles(paths []string, total int64) {
	res := a.sendFilePayload(paths)
	if a.hasRetryableFailure(res) {
		a.pushPending(pendingEntry{kind: "files", paths: paths, expires: time.Now().Add(pendingTTL)})
		log.Printf("部分节点不可达，%s 内将对在线节点重试", pendingTTL)
	}
}

// SendText 同步剪贴板文本；对端不可达时进入重试队列。
func (a *App) SendText(text string) {
	res := a.sendTextPayload(text)
	if a.hasRetryableFailure(res) {
		a.pushPending(pendingEntry{kind: "text", text: text, expires: time.Now().Add(pendingTTL)})
		log.Printf("部分节点不可达，%s 内将对在线节点重试", pendingTTL)
	}
}

func (a *App) sendFilePayload(paths []string) []SendResult {
	var payloadPath, sendName string
	var cleanup func()
	isBundle := false // true = 合成 zip（多文件/目录打包），对端将自动解包
	if len(paths) == 1 {
		fi, err := os.Stat(paths[0])
		if err != nil {
			log.Printf("无法访问 %s: %v", paths[0], err)
			return nil
		}
		if fi.IsDir() {
			log.Printf("检测到目录，正在打包 %s", paths[0])
			payloadPath, sendName, cleanup, err = bundleZip(paths)
			if err != nil {
				log.Printf("打包目录失败: %v", err)
				return nil
			}
			isBundle = true
		} else {
			payloadPath = paths[0]
			sendName = filepath.Base(payloadPath)
		}
	} else {
		log.Printf("检测到 %d 个对象，正在打包为一个 zip", len(paths))
		var err error
		payloadPath, sendName, cleanup, err = bundleZip(paths)
		if err != nil {
			log.Printf("打包失败: %v", err)
			return nil
		}
		isBundle = true
	}
	if cleanup != nil {
		defer cleanup()
	}

	sha, size, err := hashFile(payloadPath)
	if err != nil {
		log.Printf("计算文件摘要失败: %v", err)
		return nil
	}
	log.Printf("准备发送 %s（%s，sha256 %s…）到 %d 个在线节点",
		sendName, bytesize.Human(size), sha[:8], len(a.sendablePeers()))

	peers := a.sendablePeers()
	if len(peers) == 0 {
		if n := len(a.store.Alive(a.ttl())); n > 0 {
			log.Printf("跳过发送 %s：%d 个在线节点均未配对或鉴权被拒（可在面板中配对）", sendName, n)
		}
		return nil
	}

	f, err := os.Open(payloadPath)
	if err != nil {
		log.Printf("打开文件失败: %v", err)
		return nil
	}
	defer f.Close()

	var res []SendResult
	for _, p := range peers {
		hdr := transport.Header{
			V: 1, Type: "file",
			Name: sendName, Size: size, SHA256: sha, Sender: a.cfg.NodeName,
			Bundle: isBundle,
		}
		timeout := transport.TimeoutFor(size) + 30*time.Second
		// 严格按头部声明的 size 发送：文件若中途变化，宁可报错也不多发一个字节，
		// 否则接收端会带着未读数据关闭连接（RST），应答丢失表现为莫名的 EOF。
		makePayload := func() (func(io.Writer) error, error) {
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				return nil, fmt.Errorf("seek 失败: %w", err)
			}
			return func(w io.Writer) error {
				if a.sink == nil {
					_, err := io.CopyN(w, f, size)
					if errors.Is(err, io.EOF) {
						return errors.New("文件在发送途中变小，与声明的大小不一致")
					}
					return err
				}
				pw := &progressWriter{w: w, total: size, peer: p.Name, name: sendName, sink: a.sink}
				_, err := io.CopyN(pw, f, size)
				if errors.Is(err, io.EOF) {
					return errors.New("文件在发送途中变小，与声明的大小不一致")
				}
				return err
			}, nil
		}
		started := time.Now()
		resp, err := a.sendToPeer(p, hdr, makePayload, timeout)
		elapsed := time.Since(started)
		r := resultOf(p, sendName, size, resp, err)
		if r.OK && elapsed > time.Millisecond {
			log.Printf("文件 %s 已发送到 %s，耗时 %s（平均 %.1f MB/s）",
				sendName, r.Peer, elapsed.Round(time.Millisecond),
				float64(size)/elapsed.Seconds()/1e6)
		}
		res = append(res, r)
		if a.sink != nil {
			// 发送结束：无论成败都发一次终态进度（失败时面板据此收起进度条）
			done := size
			if !r.OK {
				done = -1
			}
			a.sink.Progress(Progress{Peer: p.Name, Name: sendName, Sent: done, Total: size})
		}
		a.addFile(ui.FileRecord{
			Time: time.Now(), In: false, Peer: r.Peer,
			Name: sendName, Size: size,
			Status: map[bool]string{true: "已发送", false: "失败"}[r.OK],
			Detail: r.Msg,
		})
	}
	return res
}

// progressWriter 在转发写入的同时统计字节数，按 256KB 间隔上报发送进度。
type progressWriter struct {
	w     io.Writer
	total int64
	sent  int64
	next  int64 // 下次上报的字节阈值
	peer  string
	name  string
	sink  EventSink
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	if n > 0 {
		p.sent += int64(n)
		if p.sent >= p.next {
			p.sink.Progress(Progress{Peer: p.peer, Name: p.name, Sent: p.sent, Total: p.total})
			p.next = p.sent + 256*1024
		}
	}
	return n, err
}

func (a *App) sendTextPayload(text string) []SendResult {
	data := []byte(text)
	sum := sha256.Sum256(data)
	var res []SendResult
	for _, p := range a.sendablePeers() {
		hdr := transport.Header{
			V: 1, Type: "text",
			Name: "text", Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), Sender: a.cfg.NodeName,
		}
		makePayload := func() (func(io.Writer) error, error) {
			return func(w io.Writer) error {
				_, err := w.Write(data)
				return err
			}, nil
		}
		resp, err := a.sendToPeer(p, hdr, makePayload, transport.TimeoutFor(int64(len(data)))+15*time.Second)
		r := resultOf(p, "text", int64(len(data)), resp, err)
		if r.OK {
			log.Printf("文本（%d 字符）已发送到 %s", len(text), r.Peer)
		}
		res = append(res, r)
	}
	return res
}

// authorize 是传输服务的鉴权回调：只认配对令牌——
// 发送方指纹须在信任库中，且携带的令牌等于本机签发给它的那枚。
// （共享 token 已废弃，config.json 中的旧字段不再参与鉴权。）
func (a *App) authorize(hdr transport.Header) bool {
	if hdr.SenderID == "" {
		return false
	}
	e, ok := a.trust.Get(hdr.SenderID)
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(hdr.Token), []byte(e.PairToken)) == 1
}

// sendToPeer 依次尝试节点的每个已知地址，任一成功即成功——
// 优选地址失效（如 VPN 掉线、AP 隔离）时自动换下一个，避免与节点失联。
// 只允许向已配对节点发送；被对端拒绝（配对解除）时暂停向其发送。
func (a *App) sendToPeer(p discovery.Peer, hdr transport.Header,
	makePayload func() (func(io.Writer) error, error), timeout time.Duration) (transport.Response, error) {

	var resp transport.Response
	if a.isRejected(p.ID) {
		return resp, fmt.Errorf("节点 %s 因鉴权被拒已暂停发送", p.Name)
	}
	e, ok := a.trust.Get(p.ID)
	if !ok || e.PeerToken == "" {
		return resp, fmt.Errorf("与节点 %s 未配对，请先在面板中配对", p.Name)
	}
	hdr.Token = e.PeerToken
	hdr.SenderID = a.selfID
	var lastErr error
	for _, ip := range p.IPs {
		addr := fmt.Sprintf("%s:%d", ip, p.TCPPort)
		payload, err := makePayload()
		if err != nil {
			return resp, err
		}
		log.Printf("发送 %s（%s）→ %s @ %s", hdr.Name, bytesize.Human(hdr.Size), p.Name, addr)
		resp, lastErr = transport.Send(ip, p.TCPPort, hdr, payload, timeout)
		if lastErr == nil {
			return resp, nil
		}
		if errors.Is(lastErr, transport.ErrUnauthorized) {
			a.markRejected(p)
			return resp, lastErr
		}
		log.Printf("发送到 %s @ %s 失败: %v", p.Name, addr, lastErr)
	}
	if len(p.IPs) == 0 {
		lastErr = fmt.Errorf("节点 %s 没有可用地址", p.Name)
	}
	return resp, lastErr
}

func resultOf(p discovery.Peer, name string, size int64, resp transport.Response, err error) SendResult {
	peer := p.Name + " (" + p.Primary() + ")"
	if err != nil {
		return SendResult{ID: p.ID, Peer: peer, Name: name, Size: size, OK: false, Msg: err.Error()}
	}
	msg := "ok"
	if resp.Duplicate {
		msg = "对端已存在相同内容"
	}
	return SendResult{ID: p.ID, Peer: peer, Name: name, Size: size, OK: true, Msg: msg}
}

func (a *App) ttl() time.Duration {
	return time.Duration(a.cfg.PeerTTLSec) * time.Second
}

func hasFailure(res []SendResult) bool {
	for _, r := range res {
		if !r.OK {
			return true
		}
	}
	return false
}

// hasRetryableFailure 是否存在值得重试的失败（token 不一致被暂停的节点不重试）。
func (a *App) hasRetryableFailure(res []SendResult) bool {
	for _, r := range res {
		if !r.OK && !a.isRejected(r.ID) {
			return true
		}
	}
	return false
}

// ---------- 重试队列 ----------

func (a *App) pushPending(e pendingEntry) {
	a.pendingMu.Lock()
	a.pending = append(a.pending, e)
	a.pendingMu.Unlock()
}

func (a *App) flushLoop(ctx context.Context) {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if len(a.store.Alive(a.ttl())) == 0 {
			continue
		}
		a.pendingMu.Lock()
		entries := a.pending
		a.pending = nil
		a.pendingMu.Unlock()
		if len(entries) == 0 {
			continue
		}

		now := time.Now()
		var back []pendingEntry
		for _, e := range entries {
			if now.After(e.expires) {
				log.Printf("重试超时，放弃同步之前暂存的内容")
				continue
			}
			var res []SendResult
			if e.kind == "files" {
				res = a.sendFilePayload(e.paths)
			} else {
				res = a.sendTextPayload(e.text)
			}
			if res == nil {
				back = append(back, e)
				continue
			}
			if a.hasRetryableFailure(res) {
				back = append(back, e)
			} else if hasFailure(res) {
				log.Printf("剩余失败节点均因 token 不一致被暂停，放弃重试")
			} else {
				log.Printf("暂存内容重试成功，已同步到全部在线节点")
			}
		}
		a.pendingMu.Lock()
		a.pending = append(a.pending, back...)
		a.pendingMu.Unlock()
	}
}

// ---------- 命令行辅助 ----------

// ScanPeers 在 ctx 时限内广播并收集在线节点。
func ScanPeers(ctx context.Context, cfg *config.Config) []discovery.Peer {
	id := nodeID()
	store := discovery.NewStore(id)
	store.SetLocalSubnets(localSubnets())
	interval := time.Duration(cfg.AnnounceSec) * time.Second
	go discovery.Run(ctx, cfg.DiscoveryPort, cfg.TransferPort, interval, cfg.NodeName, id, store)
	<-ctx.Done()
	return store.Alive(time.Duration(cfg.PeerTTLSec) * time.Second)
}

// SendToPeers 一次性把文件或文本发送到给定节点（不受自动同步阈值限制，不重试）。
func SendToPeers(cfg *config.Config, peers []discovery.Peer, paths []string, text string) error {
	a, err := newApp(cfg)
	if err != nil {
		return err
	}
	for _, p := range peers {
		for _, ip := range p.IPs {
			a.store.Upsert(p.ID, p.Name, ip, p.TCPPort, "")
		}
	}
	if text != "" {
		a.sendTextPayload(text)
		return nil
	}
	a.sendFilePayload(paths)
	return nil
}

// ---------- 杂项 ----------

// selfIP 返回本机展示用的首选地址（第一个非环回 IPv4）。
func selfIP() string {
	ips, _ := localAddrs()
	return ips[0]
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func printBanner(cfg *config.Config) {
	threshold := "不限制"
	if t := cfg.Threshold(); t > 0 {
		threshold = fmt.Sprintf("%d MB", cfg.MaxAutoCopyMB)
	}
	log.Printf("copywhere %s 节点 %q 已启动", Version, cfg.NodeName)
	log.Printf("发现端口 %d/udp，传输端口 %d/tcp，自动同步阈值 %s", cfg.DiscoveryPort, cfg.TransferPort, threshold)
	log.Printf("接收目录: %s", cfg.ReceiveDir)
	log.Printf("自动回贴剪贴板: %v，文本同步: %v", cfg.AutoPaste, cfg.TextSync)
	log.Printf("本机地址: %s", strings.Join(localIPs(), ", "))
	log.Printf("复制小于阈值的文件/文本即自动同步到所有在线节点；按 Ctrl+C 退出")
}

// localAddrs 返回本机所有非环回 IPv4 地址及其网段，已按地址去重。
func localAddrs() ([]string, []*net.IPNet) {
	seen := map[string]bool{}
	var ips []string
	var nets []*net.IPNet
	ifaces, err := net.Interfaces()
	if err != nil {
		return []string{"?"}, nil
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipn.IP.To4()
			if ip4 == nil {
				continue
			}
			s := ip4.String()
			if !seen[s] {
				seen[s] = true
				ips = append(ips, s)
				if len(ipn.Mask) == 4 {
					nets = append(nets, &net.IPNet{IP: ip4, Mask: ipn.Mask})
				}
			}
		}
	}
	if len(ips) == 0 {
		ips = []string{"127.0.0.1"}
	}
	return ips, nets
}

func localIPs() []string {
	ips, _ := localAddrs()
	return ips
}

func localSubnets() []*net.IPNet {
	_, nets := localAddrs()
	return nets
}
