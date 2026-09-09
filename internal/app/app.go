// Package app 组装配置、发现、传输与监控，并提供命令行入口使用的辅助函数。
package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"copywhere/internal/ui"
)

const pendingTTL = 60 * time.Second

// Options 控制 Run 的运行形态。
type Options struct {
	// Interactive 为 true 时启用 TUI（日志/在线节点/文件记录三个标签页）；
	// 为 false 时纯日志输出到标准输出。
	Interactive bool
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
	ownSeq   atomic.Uint32
	lastRecv atomic.Int64

	pendingMu sync.Mutex
	pending   []pendingEntry

	rejectedMu sync.Mutex
	rejected   map[string]bool // 因 token 不一致被暂停发送的节点（按指纹）
}

func newApp(cfg *config.Config) (*App, error) {
	store := discovery.NewStore(nodeID())
	store.SetLocalSubnets(localSubnets())
	a := &App{cfg: cfg, store: store, selfID: nodeID(), rejected: map[string]bool{}}
	store.SetRestartCallback(a.onPeerRestart)
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

// markRejected 因 token 不一致暂停向该节点发送；只提示一次。
func (a *App) markRejected(p discovery.Peer) {
	a.rejectedMu.Lock()
	defer a.rejectedMu.Unlock()
	if a.rejected[p.ID] {
		return
	}
	a.rejected[p.ID] = true
	log.Printf("节点 %q 因 token 不一致拒绝传输，已暂停向其发送（对端修正 token 并重启后将自动恢复）", p.Name)
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
	if err := precheckPorts(cfg); err != nil {
		return err
	}
	a, err := newApp(cfg)
	if err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	if opts.Interactive {
		a.ui = ui.NewController(func() []discovery.Peer { return a.store.Alive(a.ttl()) },
			a.ttl(), cfg.NodeName, selfIP())
		log.SetOutput(a.ui.Hub) // 日志进入 TUI 的日志页
	} else {
		log.SetOutput(ui.NewHub(os.Stdout)) // 纯日志模式，镜像到标准输出
	}

	go func() {
		if err := transport.Server(runCtx, cfg, a.onFileReceived, a.onTextReceived); err != nil {
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
	if cfg.KVMOn() {
		ks := kvm.NewService(
			kvm.Config{
				Port:         cfg.KVMPort,
				Left:         cfg.KVMLeft,
				Right:        cfg.KVMRight,
				EntryMonitor: cfg.KVMEntryMonitorIdx(),
			},
			cfg.NodeName, cfg.Token, a.store, a.ttl(), input.DefaultInjector{})
		if err := ks.Start(runCtx); err != nil {
			log.Printf("KVM 服务启动失败: %v", err)
		} else {
			if err := input.Start(input.Callbacks{
				OnMouseMove:     ks.OnMouseMove,
				OnMouseButton:   ks.OnMouseButton,
				OnWheel:         ks.OnWheel,
				OnKey:           ks.OnKey,
				OnLocalActivity: ks.OnLocalActivity,
			}); err != nil {
				log.Printf("输入钩子启动失败: %v", err)
			}
			log.Printf("KVM 跨屏已启用：左邻 %q，右邻 %q，端口 %d/tcp（Ctrl+Alt+Shift+X 紧急退出）",
				cfg.KVMLeft, cfg.KVMRight, cfg.KVMPort)
		}
	}
	if opts.Interactive {
		go monitor.Run(runCtx, monitor.Guard{OwnSeq: &a.ownSeq, LastReceivedAt: &a.lastRecv},
			cfg.Threshold(), cfg.TextSync, a)
		// TUI 退出（q/Esc/Ctrl+C）即整体退出
		return a.ui.Run(runCtx)
	}
	monitor.Run(runCtx, monitor.Guard{OwnSeq: &a.ownSeq, LastReceivedAt: &a.lastRecv},
		cfg.Threshold(), cfg.TextSync, a)
	return nil
}

// ---------- 接收回调 ----------

func (a *App) onFileReceived(sender, path string, dup bool) {
	a.lastRecv.Store(time.Now().UnixNano())
	if a.cfg.AutoPaste {
		if err := clip.SetFiles([]string{path}); err != nil {
			log.Printf("写入剪贴板失败: %v", err)
		} else {
			a.ownSeq.Store(clip.Seq())
		}
	}
	note := ""
	if dup {
		note = "（已存在相同内容）"
	}
	log.Printf("已接收文件 %s（来自 %s）%s，可直接 Ctrl+V", path, sender, note)
	if a.ui != nil {
		size := int64(0)
		if fi, err := os.Stat(path); err == nil {
			size = fi.Size()
		}
		status := "已接收"
		if dup {
			status = "重复内容"
		}
		a.ui.AddFile(ui.FileRecord{
			Time: time.Now(), In: true, Peer: sender,
			Name: filepath.Base(path), Size: size, Status: status, Detail: path,
		})
	}
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
			log.Printf("跳过发送 %s：所有 %d 个在线节点均因 token 不一致被暂停", sendName, n)
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
			V: 1, Token: a.cfg.Token, Type: "file",
			Name: sendName, Size: size, SHA256: sha, Sender: a.cfg.NodeName,
		}
		timeout := transport.TimeoutFor(size) + 30*time.Second
		// 严格按头部声明的 size 发送：文件若中途变化，宁可报错也不多发一个字节，
		// 否则接收端会带着未读数据关闭连接（RST），应答丢失表现为莫名的 EOF。
		makePayload := func() (func(io.Writer) error, error) {
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				return nil, fmt.Errorf("seek 失败: %w", err)
			}
			return func(w io.Writer) error {
				_, err := io.CopyN(w, f, size)
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
		if a.ui != nil {
			a.ui.AddFile(ui.FileRecord{
				Time: time.Now(), In: false, Peer: r.Peer,
				Name: sendName, Size: size,
				Status: map[bool]string{true: "已发送", false: "失败"}[r.OK],
				Detail: r.Msg,
			})
		}
	}
	return res
}

func (a *App) sendTextPayload(text string) []SendResult {
	data := []byte(text)
	sum := sha256.Sum256(data)
	var res []SendResult
	for _, p := range a.sendablePeers() {
		hdr := transport.Header{
			V: 1, Token: a.cfg.Token, Type: "text",
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

// sendToPeer 依次尝试节点的每个已知地址，任一成功即成功——
// 优选地址失效（如 VPN 掉线、AP 隔离）时自动换下一个，避免与节点失联。
// 每次尝试与失败原因都写入日志；对端以 token 不一致拒绝时暂停向其发送。
func (a *App) sendToPeer(p discovery.Peer, hdr transport.Header,
	makePayload func() (func(io.Writer) error, error), timeout time.Duration) (transport.Response, error) {

	var resp transport.Response
	if a.isRejected(p.ID) {
		return resp, fmt.Errorf("节点 %s 因 token 不一致已暂停发送", p.Name)
	}
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
