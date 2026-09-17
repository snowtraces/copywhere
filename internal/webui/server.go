package webui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"copywhere/internal/app"
	"copywhere/internal/config"
	"copywhere/internal/diag"
	"copywhere/internal/discovery"
	"copywhere/internal/trust"
)

// Core 是面板对运行中服务的操作接口（由 *app.App 实现）。
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

// Server 是本地控制面板 HTTP 服务（仅监听 127.0.0.1）。
type Server struct {
	bus     *Bus
	core    Core
	cfg     *config.Config
	cfgPath string
	token   string
	url     string
	port    int
	srv     *http.Server

	stackMu       sync.Mutex // 栈转储限流状态
	lastStackDump time.Time
}

// peerJSON 是 discovery.Peer 的 JSON 视图（附带上一次活跃的相对时间与配对状态）。
type peerJSON struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	IPs     []string `json:"ips"`
	TCPPort int      `json:"tcp_port"`
	Idle    string   `json:"idle"`
	Paired  bool     `json:"paired"`
	Blocked bool     `json:"blocked"` // 曾因鉴权被拒暂停发送（对方解除配对等）
}

type stateJSON struct {
	NodeName    string `json:"node_name"`
	SelfIP      string `json:"self_ip"`
	Version     string `json:"version"`
	Paused      bool   `json:"paused"`
	ReceiveDir  string `json:"receive_dir"`
	ThresholdMB int64  `json:"threshold_mb"`
	AutoPaste   bool   `json:"auto_paste"`
	TextSync    bool   `json:"text_sync"`
	PeerCount   int    `json:"peer_count"`
	KVMEnabled  bool   `json:"kvm_enabled"`
	KVMTouchpad bool   `json:"kvm_touchpad_gestures"`
	KVMLeft     string `json:"kvm_left"`
	KVMRight    string `json:"kvm_right"`
	WatchMode   string `json:"watch_mode"` // 剪贴板监听方式：listener | viewer | none
}

// NewServer 创建面板服务：生成访问 token 并绑定端口。
// 端口优先取 cfg.WebPort；被占用或未配置（<=0）时回退随机端口。
func NewServer(core Core, cfg *config.Config, cfgPath string, bus *Bus) (*Server, error) {
	token, err := randomToken()
	if err != nil {
		return nil, fmt.Errorf("生成面板访问令牌失败: %w", err)
	}
	s := &Server{bus: bus, core: core, cfg: cfg, cfgPath: cfgPath, token: token}

	port := cfg.WebPort
	if port > 0 {
		ln, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			log.Printf("面板端口 %d 被占用，改用随机端口", port)
			port = 0
		} else {
			ln.Close()
		}
	}
	ln, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, fmt.Errorf("面板监听失败: %w", err)
	}
	s.url = fmt.Sprintf("http://127.0.0.1:%d/?key=%s", ln.Addr().(*net.TCPAddr).Port, token)
	s.port = ln.Addr().(*net.TCPAddr).Port
	s.srv = &http.Server{Handler: s.routes(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := s.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("面板服务异常退出: %v", err)
		}
	}()
	return s, nil
}

// URL 返回带访问令牌的面板地址（托盘"打开面板"用）。
func (s *Server) URL() string { return s.url }

// Shutdown 优雅关闭面板服务。
func (s *Server) Shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s.srv.Shutdown(ctx)
}

// StartSnapshotLoop 周期性向订阅者推送节点表与状态快照，直到 ctx 结束。
func (s *Server) StartSnapshotLoop(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	go func() {
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if s.bus.Subscribers() == 0 {
				continue
			}
			s.bus.publish(Event{Type: EvPeers, Data: s.peersView()})
			s.bus.publish(Event{Type: EvState, Data: s.stateView()})
		}
	}()
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/state", s.apiState)
	mux.HandleFunc("GET /api/peers", s.apiPeers)
	mux.HandleFunc("GET /api/logs", s.apiLogs)
	mux.HandleFunc("GET /api/records", s.apiRecords)
	mux.HandleFunc("GET /api/config", s.apiGetConfig)
	mux.HandleFunc("POST /api/config", s.apiSetConfig)
	mux.HandleFunc("GET /api/events", s.apiEvents)
	mux.HandleFunc("POST /api/pause", s.apiPause)
	mux.HandleFunc("POST /api/send-text", s.apiSendText)
	mux.HandleFunc("POST /api/upload", s.apiUpload)
	mux.HandleFunc("POST /api/open", s.apiOpen)
	mux.HandleFunc("POST /api/pair", s.apiPair)
	mux.HandleFunc("GET /api/pair/pending", s.apiPairPending)
	mux.HandleFunc("POST /api/pair/respond", s.apiPairRespond)
	mux.HandleFunc("GET /api/paired", s.apiPairedList)
	mux.HandleFunc("POST /api/unpair", s.apiUnpair)
	mux.HandleFunc("GET /api/file", s.apiFile)
	mux.HandleFunc("GET /api/debug", s.apiDebug)
	mux.HandleFunc("POST /api/debug/stack", s.apiDebugStack)
	mux.HandleFunc("GET /", s.apiIndex)
	return s.withAuth(mux)
}

// withAuth 校验访问令牌：cookie 优先，其次 URL 参数（首次由托盘带入后种 cookie）。
// /favicon.* 豁免鉴权：仅静态图标，且浏览器首拉时可能尚无 cookie。
func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/favicon.ico":
			w.Header().Set("Content-Type", "image/x-icon")
			w.Header().Set("Cache-Control", "public, max-age=86400")
			w.Write(iconICO)
			return
		case "/favicon.svg":
			w.Header().Set("Content-Type", "image/svg+xml")
			w.Header().Set("Cache-Control", "public, max-age=86400")
			w.Write(faviconSVG)
			return
		case "/favicon.png":
			w.Header().Set("Content-Type", "image/png")
			w.Header().Set("Cache-Control", "public, max-age=86400")
			w.Write(faviconPNG)
			return
		}
		if c, err := r.Cookie("cwkey"); err == nil && c.Value == s.token {
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Query().Get("key") == s.token {
			http.SetCookie(w, &http.Cookie{
				Name: "cwkey", Value: s.token, Path: "/",
				MaxAge: 30 * 24 * 3600, HttpOnly: true, SameSite: http.SameSiteLaxMode,
			})
			next.ServeHTTP(w, r)
			return
		}
		http.Error(w, "缺少访问令牌：请从托盘菜单重新打开面板", http.StatusUnauthorized)
	})
}

// ---------- JSON 视图 ----------

func (s *Server) peersView() []peerJSON {
	peers := s.core.Peers()
	out := make([]peerJSON, len(peers))
	for i, p := range peers {
		out[i] = peerJSON{
			ID: p.ID, Name: p.Name, IPs: p.IPs, TCPPort: p.TCPPort,
			Idle:    time.Since(p.LastSeen).Round(time.Second).String(),
			Paired:  s.core.PairedWith(p.ID),
			Blocked: s.core.RejectedWith(p.ID),
		}
	}
	return out
}

func (s *Server) stateView() stateJSON {
	return stateJSON{
		NodeName:    s.cfg.NodeName,
		SelfIP:      s.core.SelfIP(),
		Version:     app.Version,
		Paused:      s.core.IsPaused(),
		ReceiveDir:  s.cfg.ReceiveDir,
		ThresholdMB: s.cfg.MaxAutoCopyMB,
		AutoPaste:   s.cfg.AutoPaste,
		TextSync:    s.cfg.TextSync,
		PeerCount:   len(s.core.Peers()),
		KVMEnabled:  s.cfg.KVMOn(),
		KVMTouchpad: s.cfg.KVMTouchpadOn(),
		KVMLeft:     s.cfg.KVMLeft,
		KVMRight:    s.cfg.KVMRight,
		WatchMode:   s.core.WatchMode(),
	}
}

// ---------- REST 处理器 ----------

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

func (s *Server) apiState(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.stateView()) }

func (s *Server) apiPeers(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.peersView()) }

func (s *Server) apiLogs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"lines": s.bus.Logs()})
}

func (s *Server) apiRecords(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"records": s.bus.Files()})
}

// apiDebug 输出运行时诊断快照：原子计数器、心跳年龄、活动会话、
// 环形诊断日志，外加节点表与关键配置摘要。用于排查"活着但不干活"类问题。
// 浏览器地址栏直接访问（HTML 请求）时渲染成自动刷新的页面，
// 面板与 API 客户端（默认）拿到 JSON。
func (s *Server) apiDebug(w http.ResponseWriter, r *http.Request) {
	payload := map[string]any{
		"version":     app.Version,
		"snapshot":    diag.Snap(),
		"peers":       s.peersView(),
		"state":       s.stateView(),
		"receive_dir": s.cfg.ReceiveDir,
		"config_path": s.cfgPath,
		"listen": map[string]any{
			"discovery_udp": s.cfg.DiscoveryPort,
			"transfer_tcp":  s.cfg.TransferPort,
			"kvm_tcp":       s.cfg.KVMPort,
			"panel_tcp":     fmt.Sprintf("127.0.0.1:%d", s.port),
		},
	}
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		b, err := json.MarshalIndent(payload, "", "  ")
		view := []byte("序列化诊断快照失败: " + fmt.Sprint(err))
		if err == nil {
			// 显式转义后才进 <pre>：不依赖 json.Marshal 的隐式 HTML 转义
			// （快照含对端可控的节点名等文本，防存储型 XSS）。
			view = []byte(html.EscapeString(string(b)))
		}
		fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>copywhere 诊断</title>
<meta http-equiv="refresh" content="3"><style>body{background:#111827;color:#e5e7eb;font:13px/1.6 ui-monospace,Consolas,monospace;padding:24px}pre{white-space:pre-wrap}button{background:#2563eb;color:#fff;border:0;border-radius:6px;padding:4px 12px;cursor:pointer;font:inherit}</style>
<body><h3 style="color:#93c5fd">copywhere 运行时诊断（3 秒自动刷新）
<a href="/" style="color:#6b7280;font-size:12px;margin-left:12px">返回面板</a>
<button style="margin-left:12px" onclick="fetch('/api/debug/stack',{method:'POST'}).then(()=>setTimeout(()=>location.reload(),300))">转储堆栈</button></h3><pre>%s</pre>`, view)
		return
	}
	writeJSON(w, payload)
}

// apiDebugStack 立即转储全部 goroutine 堆栈到诊断环并返回快照。
// 面板「转储堆栈」按钮调用，是定位卡死/死锁的第一手材料。
// 全量栈转储开销大（1MB 缓冲 + 环写），限流 2 秒一次防误触刷爆诊断环。
func (s *Server) apiDebugStack(w http.ResponseWriter, r *http.Request) {
	s.stackMu.Lock()
	if time.Since(s.lastStackDump) < 2*time.Second {
		s.stackMu.Unlock()
		writeJSON(w, map[string]any{"ok": false, "error": "转储过于频繁，请 2 秒后再试", "dumped": 0})
		return
	}
	s.lastStackDump = time.Now()
	s.stackMu.Unlock()
	n := diag.DumpStacks()
	writeJSON(w, map[string]any{"ok": true, "dumped": n, "snapshot": diag.Snap()})
}

func (s *Server) apiGetConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"config": s.cfg,
		"path":   s.cfgPath,
		"hint":   "所有配置修改后立即保存并即时生效，无需重启（KVM 邻居会自动同步到对端；端口类字段面板未开放编辑）",
	})
}

// apiSetConfig 更新并保存配置：所有字段即时生效（KVM 开关热启停、
// 节点名下一次广播生效、邻居自动同步到对端），无需重启。
func (s *Server) apiSetConfig(w http.ResponseWriter, r *http.Request) {
	var in map[string]any
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "请求体不是合法 JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	var errs []string
	applyStr := func(key string, dst *string) {
		if v, ok := in[key]; ok {
			sv, ok := v.(string)
			if !ok || strings.TrimSpace(sv) == "" {
				errs = append(errs, key+" 需要非空字符串")
				return
			}
			*dst = sv
		}
	}
	applyStr("receive_dir", &s.cfg.ReceiveDir)
	nodeNameChanged := false
	if v, ok := in["node_name"]; ok {
		if sv, ok := v.(string); ok && strings.TrimSpace(sv) != "" {
			if s.cfg.NodeName != sv {
				nodeNameChanged = true
			}
			s.cfg.NodeName = sv
		}
	}
	if v, ok := in["max_auto_copy_mb"]; ok {
		if fv, ok := v.(float64); ok && fv >= 0 {
			s.cfg.MaxAutoCopyMB = int64(fv)
		} else {
			errs = append(errs, "max_auto_copy_mb 需要非负数字")
		}
	}
	if v, ok := in["auto_paste"]; ok {
		if bv, ok := v.(bool); ok {
			s.cfg.AutoPaste = bv
		} else {
			errs = append(errs, "auto_paste 需要布尔值")
		}
	}
	if v, ok := in["text_sync"]; ok {
		if bv, ok := v.(bool); ok {
			s.cfg.TextSync = bv
		} else {
			errs = append(errs, "text_sync 需要布尔值")
		}
	}
	// KVM 布局：允许空串（清除某一侧）；邻居名本机即时热更新，并推送到对端。
	// 记录改动前的值：取消（清空）或换人时向旧邻居推送移除通知。
	oldKVMLeft, oldKVMRight := s.cfg.KVMLeft, s.cfg.KVMRight
	kvmChanged := false
	applyKVM := func(key string, dst *string) {
		if v, ok := in[key]; ok {
			sv, ok := v.(string)
			if !ok {
				errs = append(errs, key+" 需要字符串")
				return
			}
			*dst = strings.TrimSpace(sv)
			kvmChanged = true
		}
	}
	applyKVM("kvm_left", &s.cfg.KVMLeft)
	applyKVM("kvm_right", &s.cfg.KVMRight)
	kvmEnableChanged := false
	if v, ok := in["kvm_enabled"]; ok {
		if bv, ok := v.(bool); ok {
			if s.cfg.KVMOn() != bv {
				kvmEnableChanged = true
			}
			s.cfg.KVMEnabled = &bv
		} else {
			errs = append(errs, "kvm_enabled 需要布尔值")
		}
	}
	// 触控板手势识别（实验性）：开关与速度倍率均即时生效，无需重启
	touchpadChanged := false
	if v, ok := in["kvm_touchpad_gestures"]; ok {
		if bv, ok := v.(bool); ok {
			if s.cfg.KVMTouchpadOn() != bv {
				touchpadChanged = true
			}
			s.cfg.KVMTouchpad = &bv
		} else {
			errs = append(errs, "kvm_touchpad_gestures 需要布尔值")
		}
	}
	if v, ok := in["kvm_touchpad_speed"]; ok {
		if fv, ok := v.(float64); ok && fv >= 10 && fv <= 1000 {
			if s.cfg.KVMTouchpadSpeedPct() != int(fv) {
				touchpadChanged = true
			}
			s.cfg.KVMTouchSpd = int(fv)
		} else {
			errs = append(errs, "kvm_touchpad_speed 需要 10~1000 的整数（100=基准）")
		}
	}
	// KVM 手感调参：主控移动合拍间隔、被控重排节拍（毫秒）。
	// 与邻居一致走热更新通道（保存即触发 core.SyncKVM → kvm.UpdateTunables），
	// 无需重启。move>=0（0=默认8），reflow 可为 -1（关闭重排）。
	if v, ok := in["kvm_move_interval_ms"]; ok {
		if fv, ok := v.(float64); ok && fv >= 0 {
			s.cfg.KVMMoveMs = int(fv)
			kvmChanged = true
		} else {
			errs = append(errs, "kvm_move_interval_ms 需要非负整数")
		}
	}
	if v, ok := in["kvm_reflow_step_ms"]; ok {
		if fv, ok := v.(float64); ok && fv >= -1 {
			s.cfg.KVMReflowMs = int(fv)
			kvmChanged = true
		} else {
			errs = append(errs, "kvm_reflow_step_ms 需要 -1 或非负整数")
		}
	}
	if v, ok := in["kvm_speed_percent"]; ok {
		if fv, ok := v.(float64); ok && fv >= 0 && fv <= 500 {
			s.cfg.KVMSpeedPct = int(fv)
			kvmChanged = true
		} else {
			errs = append(errs, "kvm_speed_percent 需要 0~500 的整数（100=基准 1.0x）")
		}
	}
	if len(errs) > 0 {
		http.Error(w, strings.Join(errs, "；"), http.StatusBadRequest)
		return
	}
	if err := s.cfg.Save(s.cfgPath); err != nil {
		http.Error(w, "保存配置失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("配置已通过面板更新并保存")
	if nodeNameChanged {
		// 广播名每次公告时取当前值：下一次广播（约 announce_interval 秒）即生效
		log.Printf("节点名已更新为 %q，将在下一次广播生效", s.cfg.NodeName)
	}
	if kvmEnableChanged {
		// 跨屏开关即时生效：热启动/停用 KVM 服务与监听，无需重启
		s.core.SetKVMEnabled(s.cfg.KVMOn())
	}
	if touchpadChanged {
		// 触控板手势识别与速度倍率即时生效（实验性），无需重启
		s.core.SetTouchpadGestures(s.cfg.KVMTouchpadOn())
		s.core.SetTouchpadSpeed(s.cfg.KVMTouchpadSpeedPct())
	}
	if kvmChanged {
		// 邻居改动即时生效（本机热更新 + 推送到对端，自动互为镜像；
		// 取消/换人时同步通知旧邻居解除）
		go s.core.SyncKVMFrom(oldKVMLeft, oldKVMRight)
	}
	writeJSON(w, map[string]any{"ok": true})
}

// apiPause 切换自动同步开关。
func (s *Server) apiPause(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Paused bool `json:"paused"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "请求体不是合法 JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.core.SetPaused(in.Paused)
	writeJSON(w, map[string]any{"ok": true, "paused": s.core.IsPaused()})
}

// apiSendText 手动发送文本到所有在线节点（不受阈值限制）。
func (s *Server) apiSendText(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "请求体不是合法 JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if in.Text == "" {
		http.Error(w, "text 不能为空", http.StatusBadRequest)
		return
	}
	if len(s.core.Peers()) == 0 {
		http.Error(w, "未发现在线节点", http.StatusConflict)
		return
	}
	go s.core.SendText(in.Text)
	writeJSON(w, map[string]any{"ok": true})
}

// apiUpload 接收面板拖入/选择的文件，落盘临时目录后在后台手动发送
// （等价 send 命令，不受阈值限制），发送完成后清理临时目录。
func (s *Server) apiUpload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		http.Error(w, "解析上传失败: "+err.Error(), http.StatusBadRequest)
		return
	}
	form := r.MultipartForm
	if form == nil || len(form.File["files"]) == 0 {
		http.Error(w, "未收到任何文件（字段名须为 files）", http.StatusBadRequest)
		return
	}
	tmpDir, err := os.MkdirTemp("", "copywhere-upload-")
	if err != nil {
		http.Error(w, "创建临时目录失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	var paths []string
	var total int64
	for _, fh := range form.File["files"] {
		dst := filepath.Join(tmpDir, filepath.Base(fh.Filename))
		f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			http.Error(w, "保存文件失败: "+err.Error(), http.StatusInternalServerError)
			return
		}
		src, err := fh.Open()
		if err != nil {
			f.Close()
			http.Error(w, "读取上传失败: "+err.Error(), http.StatusInternalServerError)
			return
		}
		n, err := copyAndClose(src, f)
		src.Close()
		if err != nil {
			http.Error(w, "保存文件失败: "+err.Error(), http.StatusInternalServerError)
			return
		}
		paths = append(paths, dst)
		total += n
	}
	if len(s.core.Peers()) == 0 {
		os.RemoveAll(tmpDir)
		http.Error(w, "未发现在线节点", http.StatusConflict)
		return
	}
	log.Printf("面板上传 %d 个文件（%s），开始发送", len(paths), bytesHuman(total))
	go func() {
		defer os.RemoveAll(tmpDir)
		s.core.SendFiles(paths, total)
	}()
	writeJSON(w, map[string]any{"ok": true, "count": len(paths), "total": total})
}

// apiPair 主动向指定节点发起配对（同步等待对端裁决）。
func (s *Server) apiPair(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.ID == "" {
		http.Error(w, "缺少 id", http.StatusBadRequest)
		return
	}
	if err := s.core.PairWith(in.ID); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// apiPairPending 返回当前待裁决的配对请求（面板刷新后恢复弹窗）。
func (s *Server) apiPairPending(w http.ResponseWriter, r *http.Request) {
	id, name, ok := s.core.PendingPair()
	if !ok {
		writeJSON(w, map[string]any{"pending": nil})
		return
	}
	writeJSON(w, map[string]any{"pending": map[string]string{"id": id, "name": name}})
}

// apiPairRespond 本机用户对配对请求做出裁决。
func (s *Server) apiPairRespond(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Accept bool `json:"accept"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "请求体不是合法 JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.core.RespondPair(in.Accept); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "accepted": in.Accept})
}

// apiPairedList 返回配对令牌列表（不含令牌本身）。
func (s *Server) apiPairedList(w http.ResponseWriter, r *http.Request) {
	list := s.core.PairedList()
	type row struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		PairedAt string `json:"paired_at"`
	}
	out := make([]row, len(list))
	for i, e := range list {
		out[i] = row{ID: e.ID, Name: e.Name, PairedAt: e.PairedAt.Format("2006-01-02 15:04")}
	}
	writeJSON(w, map[string]any{"paired": out})
}

// apiUnpair 解除与指定节点的配对（吊销签发给它的令牌）。
func (s *Server) apiUnpair(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.ID == "" {
		http.Error(w, "缺少 id", http.StatusBadRequest)
		return
	}
	if err := s.core.Unpair(in.ID); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// apiOpen 在资源管理器中打开路径：{"path": "...", "select": true} 高选中文件，
// 或 {"dir": "receive"} 打开接收目录。
func (s *Server) apiOpen(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Path   string `json:"path"`
		Dir    string `json:"dir"`
		Select bool   `json:"select"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "请求体不是合法 JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	target := in.Path
	if in.Dir == "receive" {
		target = s.cfg.ReceiveDir
	}
	if target == "" {
		http.Error(w, "缺少 path 或 dir", http.StatusBadRequest)
		return
	}
	if _, err := os.Stat(target); err != nil {
		http.Error(w, "路径不存在: "+target, http.StatusNotFound)
		return
	}
	if err := RevealInExplorer(target, in.Select); err != nil {
		http.Error(w, "打开失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// apiEvents 是 SSE 端点：只推实时事件，历史数据由各 REST 接口提供。
func (s *Server) apiEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE 不受支持", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	ch, cancel := s.bus.Subscribe()
	defer cancel()
	// 立即推一次快照，客户端无需等待周期推送
	s.bus.publishTo(map[chan Event]struct{}{ch: {}}, Event{Type: EvPeers, Data: s.peersView()})
	s.bus.publishTo(map[chan Event]struct{}{ch: {}}, Event{Type: EvState, Data: s.stateView()})
	fl.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			data, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

// imageExts 列出允许预览的图片扩展名 → Content-Type。
var imageExts = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".gif": "image/gif", ".bmp": "image/bmp", ".webp": "image/webp",
}

// apiFile 为面板提供接收目录内图片的缩略图预览数据源。
// 安全边界：面板本身仅监听 127.0.0.1 且需访问令牌；此处再限定
// 只允许「接收目录内 + 常见图片扩展名」的文件，杜绝任意文件读取。
func (s *Server) apiFile(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	if p == "" {
		http.Error(w, "缺少 path", http.StatusBadRequest)
		return
	}
	ct, ok := imageExts[strings.ToLower(filepath.Ext(p))]
	if !ok {
		http.Error(w, "仅支持图片预览", http.StatusBadRequest)
		return
	}
	root, err := filepath.Abs(s.cfg.ReceiveDir)
	if err != nil || root == "" {
		http.Error(w, "接收目录不可用", http.StatusForbidden)
		return
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		http.Error(w, "路径非法", http.StatusBadRequest)
		return
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		http.Error(w, "仅允许预览接收目录内的文件", http.StatusForbidden)
		return
	}
	f, err := os.Open(abs)
	if err != nil {
		http.Error(w, "文件不存在", http.StatusNotFound)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "private, max-age=3600")
	io.Copy(w, f)
}

func (s *Server) apiIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(indexHTML)
}

func randomToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// bytesHuman 是 bytesize 的本地简化版（webui 不引入 bytesize 以保持依赖面最小）。
func bytesHuman(n int64) string {
	const kb, mb = 1 << 10, 1 << 20
	switch {
	case n >= mb:
		return fmt.Sprintf("%.1fMB", float64(n)/mb)
	case n >= kb:
		return fmt.Sprintf("%.1fKB", float64(n)/kb)
	default:
		return fmt.Sprintf("%dB", n)
	}
}
