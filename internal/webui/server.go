package webui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"copywhere/internal/app"
	"copywhere/internal/config"
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
	// KVM 布局：本机热更新 + 推送到对端（互为镜像）
	SyncKVM()
}

// Server 是本地控制面板 HTTP 服务（仅监听 127.0.0.1）。
type Server struct {
	bus     *Bus
	core    Core
	cfg     *config.Config
	cfgPath string
	token   string
	url     string
	srv     *http.Server
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
	KVMLeft     string `json:"kvm_left"`
	KVMRight    string `json:"kvm_right"`
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
		KVMLeft:     s.cfg.KVMLeft,
		KVMRight:    s.cfg.KVMRight,
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

func (s *Server) apiGetConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"config": s.cfg,
		"path":   s.cfgPath,
		"hint":   "端口、node_name 与 KVM 开关修改后需重启 copywhere 生效；其余字段保存后立即生效（KVM 邻居会自动同步到对端）",
	})
}

// apiSetConfig 更新并保存配置；可即时生效的字段直接改内存配置，
// 需要重启的字段只写盘并在应答中提示。
func (s *Server) apiSetConfig(w http.ResponseWriter, r *http.Request) {
	var in map[string]any
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "请求体不是合法 JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	var restart []string
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
	if v, ok := in["node_name"]; ok {
		if sv, ok := v.(string); ok && strings.TrimSpace(sv) != "" {
			s.cfg.NodeName = sv
			restart = append(restart, "node_name")
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
	// KVM 布局：允许空串（清除某一侧）；邻居名本机即时热更新，并推送到对端
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
	if v, ok := in["kvm_enabled"]; ok {
		if bv, ok := v.(bool); ok {
			s.cfg.KVMEnabled = &bv
			restart = append(restart, "kvm_enabled")
		} else {
			errs = append(errs, "kvm_enabled 需要布尔值")
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
	if kvmChanged {
		// 邻居改动即时生效（本机热更新 + 推送到对端，自动互为镜像）
		go s.core.SyncKVM()
	}
	writeJSON(w, map[string]any{"ok": true, "restart_required": restart})
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
