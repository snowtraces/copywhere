// Package transport 实现节点间的 TCP 传输协议（服务端 + 客户端）。
//
// 协议：单行 JSON 头（\n 结尾）+ 原始负载 + 单行 JSON 应答。
// 文件先落盘为临时文件，校验 sha256 后再改名；每次接收落在独立的
// 子目录（时间戳 + 发送方命名），文件保留原始文件名，不做重命名。
package transport

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
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
	"time"

	"copywhere/internal/config"
)

const (
	maxFileBytes = int64(4) << 30 // 单文件上限 4GB
	maxTextBytes = int64(4) << 20 // 文本上限 4MB

	// 超时预算：底数 60s，之后按 256KB/s 估算传输时间。
	// ZeroTier 中继、弱 WiFi 等慢链路可能远低于 1MB/s，预算必须宽裕。
	xferChunkBytes = int64(256 * 1024)
)

// TimeoutFor 返回传输 size 字节的服务端预算时长。
func TimeoutFor(size int64) time.Duration {
	return time.Duration(60+size/xferChunkBytes) * time.Second
}

// Header 是每次传输的 JSON 头。
type Header struct {
	V        int    `json:"v"`                   // 协议版本
	Token    string `json:"token"`               // 鉴权令牌（对方签发给本节点的配对令牌）
	Type     string `json:"type"`                // "file" | "text" | "pair" | "kvm"
	Name     string `json:"name"`                // 文件名（text/kvm 时为固定标识）
	Size     int64  `json:"size"`                // 负载字节数
	SHA256   string `json:"sha256"`              // 负载 sha256
	Sender   string `json:"sender"`              // 发送方节点名
	SenderID string `json:"sender_id,omitempty"` // 发送方节点指纹（配对令牌校验依赖它）
	// Bundle 为 true 表示负载是发送端因多文件/目录而临时打包的合成 zip，
	// 接收端可自动解包；用户亲手发送的单个 zip 文件不置位，原样保留。
	Bundle bool `json:"bundle,omitempty"`
}

// PairOffer 是配对请求负载：发起方出示自己的身份与签发给对端的令牌。
type PairOffer struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Token string `json:"token"`
}

// PairDecision 是被控方对配对请求的裁决。
// 接受时 Token 为被控方新签发给发起方的令牌，发起方之后发送内容须携带它。
type PairDecision struct {
	Accepted bool
	Token    string
	ID       string
	Name     string
}

// KVMSync 是 KVM 布局同步负载：发送方告知接收方
// "把我放到你的 Side 侧"（Side 为接收方视角的 left/right）。
type KVMSync struct {
	PeerID   string `json:"peer_id"`
	PeerName string `json:"peer_name"`
	Side     string `json:"side"` // "left" | "right"
}

// Response 是接收方的 JSON 应答。
type Response struct {
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
	Path      string `json:"path,omitempty"`      // 接收后的最终路径
	Duplicate bool   `json:"duplicate,omitempty"` // 内容已存在
	Token     string `json:"token,omitempty"`     // 配对接受时：对端签发的令牌
	PeerID    string `json:"peer_id,omitempty"`   // 配对接受时：对端节点指纹
	PeerName  string `json:"peer_name,omitempty"` // 配对接受时：对端节点名
}

// Handlers 是传输服务的回调集合。
type Handlers struct {
	// OnFile 收到文件（已落盘校验通过）。bundle 表示该文件是发送端的合成 zip。
	OnFile func(sender, path string, dup bool, bundle bool)
	// OnText 收到文本。
	OnText func(sender, text string)
	// Authorize 鉴权回调（必填）：校验配对令牌，返回 false 即拒绝。
	Authorize func(hdr Header) bool
	// OnPair 处理配对请求：校验并等待本机用户裁决（可阻塞），
	// 接受时返回已签发的 PairDecision，拒绝时返回 Accepted=false。
	OnPair func(offer PairOffer) PairDecision
	// OnKVM 处理布局同步：把发送方登记为指定侧的 KVM 邻居并即时生效。
	OnKVM func(sync KVMSync) error
}

// Server 启动 TCP 传输服务，直到 ctx 结束。
func Server(ctx context.Context, cfg *config.Config, h Handlers) error {

	ln, err := net.Listen("tcp4", fmt.Sprintf("0.0.0.0:%d", cfg.TransferPort))
	if err != nil {
		return fmt.Errorf("监听传输端口 %d/tcp 失败: %w", cfg.TransferPort, err)
	}
	go func() { <-ctx.Done(); ln.Close() }()
	log.Printf("传输服务已监听 :%d/tcp", cfg.TransferPort)

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go handle(conn, cfg, h)
	}
}

func handle(conn net.Conn, cfg *config.Config, h Handlers) {

	defer conn.Close()
	peer := conn.RemoteAddr().String()
	conn.SetDeadline(time.Now().Add(30 * time.Second)) // 仅限读取协议头
	br := bufio.NewReaderSize(conn, 64*1024)

	line, err := br.ReadSlice('\n')
	if err != nil {
		log.Printf("接收 %s 的协议头失败: %v", peer, err)
		writeResp(conn, Response{Error: "bad header"})
		return
	}
	var hdr Header
	if json.Unmarshal(bytes.TrimRight(line, "\r\n"), &hdr) != nil {
		writeResp(conn, Response{Error: "bad header"})
		return
	}
	if hdr.V != 1 {
		writeResp(conn, Response{Error: "unsupported version"})
		return
	}
	// 配对请求自成一类：鉴权由配对流程本身（人工确认）完成
	if hdr.Type == "pair" {
		handlePair(conn, br, hdr, h, peer)
		return
	}
	authorized := false
	if h.Authorize != nil {
		authorized = h.Authorize(hdr)
	}
	if !authorized {
		log.Printf("拒绝 %s 的连接（未配对或令牌无效）", peer)
		writeResp(conn, Response{Error: "unauthorized"})
		return
	}
	switch hdr.Type {
	case "file":
		recvFile(conn, br, hdr, cfg, h.OnFile, peer)
	case "text":
		recvText(conn, br, hdr, h.OnText, peer)
	case "kvm":
		handleKVM(conn, br, hdr, h, peer)
	default:
		writeResp(conn, Response{Error: "unknown type"})
	}
}

// handleKVM 处理布局同步：负载为 KVMSync（已在鉴权通过后到达）。
func handleKVM(conn net.Conn, br *bufio.Reader, hdr Header, h Handlers, peer string) {
	if h.OnKVM == nil {
		writeResp(conn, Response{Error: "kvm sync unsupported"})
		return
	}
	if hdr.Size <= 0 || hdr.Size > 64*1024 {
		writeResp(conn, Response{Error: "size out of range"})
		return
	}
	b, err := io.ReadAll(io.LimitReader(br, hdr.Size))
	if err != nil || int64(len(b)) != hdr.Size {
		writeResp(conn, Response{Error: "transfer incomplete"})
		return
	}
	var kv KVMSync
	if json.Unmarshal(b, &kv) != nil || kv.PeerID == "" || kv.PeerName == "" {
		writeResp(conn, Response{Error: "bad kvm sync"})
		return
	}
	if err := h.OnKVM(kv); err != nil {
		writeResp(conn, Response{Error: err.Error()})
		return
	}
	writeResp(conn, Response{OK: true})
}

// handlePair 处理配对请求：读取负载（发起方出示的身份与令牌），
// 交给 OnPair 等待本机用户裁决，再沿原连接应答（应答无法被第三方伪造）。
func handlePair(conn net.Conn, br *bufio.Reader, hdr Header, h Handlers, peer string) {
	if h.OnPair == nil {
		writeResp(conn, Response{Error: "pair unsupported"})
		return
	}
	if hdr.Size <= 0 || hdr.Size > 64*1024 {
		writeResp(conn, Response{Error: "size out of range"})
		return
	}
	// 配对需要人工确认，放宽读取截止时间
	conn.SetDeadline(time.Now().Add(3 * time.Minute))
	b, err := io.ReadAll(io.LimitReader(br, hdr.Size))
	if err != nil || int64(len(b)) != hdr.Size {
		writeResp(conn, Response{Error: "transfer incomplete"})
		return
	}
	var offer PairOffer
	if json.Unmarshal(b, &offer) != nil || offer.ID == "" || offer.Token == "" {
		writeResp(conn, Response{Error: "bad pair offer"})
		return
	}
	log.Printf("收到节点 %q（指纹 %s…）的配对请求", offer.Name, shortID(offer.ID))
	d := h.OnPair(offer)
	if !d.Accepted {
		log.Printf("已拒绝节点 %q 的配对请求", offer.Name)
		writeResp(conn, Response{Error: "pair rejected"})
		return
	}
	log.Printf("已接受节点 %q 的配对请求", d.Name)
	writeResp(conn, Response{OK: true, Token: d.Token, PeerID: d.ID, PeerName: d.Name})
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func recvFile(conn net.Conn, br *bufio.Reader, hdr Header, cfg *config.Config,
	onFile func(sender, path string, dup bool, bundle bool), peer string) {

	if hdr.Size < 0 || hdr.Size > maxFileBytes {
		writeResp(conn, Response{Error: "size out of range"})
		return
	}
	// 每次接收独占一个子目录（时间戳 + 发送方），文件保留原始文件名，
	// 不再对重名文件追加 " (n)" 之类的改名。
	if err := os.MkdirAll(cfg.ReceiveDir, 0o755); err != nil {
		writeResp(conn, Response{Error: "mkdir: " + err.Error()})
		return
	}
	dir, err := newTransferDir(cfg.ReceiveDir, hdr.Sender)
	if err != nil {
		writeResp(conn, Response{Error: "mkdir: " + err.Error()})
		return
	}
	conn.SetDeadline(time.Now().Add(TimeoutFor(hdr.Size)))

	tmp := filepath.Join(dir, ".cw-partial-"+randHex(6))
	out, err := os.Create(tmp)
	if err != nil {
		removeDirIfEmpty(dir)
		writeResp(conn, Response{Error: "create temp: " + err.Error()})
		return
	}
	h := sha256.New()
	n, cerr := io.Copy(io.MultiWriter(out, h), io.LimitReader(br, hdr.Size))
	out.Close()

	if cerr != nil || n != hdr.Size {
		os.Remove(tmp)
		removeDirIfEmpty(dir)
		log.Printf("接收 %s 的文件 %q 不完整: got %d/%d bytes, err=%v（多为链路过慢触发超时或对端中断）",
			peer, hdr.Name, n, hdr.Size, cerr)
		writeResp(conn, Response{Error: fmt.Sprintf("transfer incomplete: got %d/%d bytes", n, hdr.Size)})
		return
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if sum != hdr.SHA256 {
		os.Remove(tmp)
		removeDirIfEmpty(dir)
		log.Printf("接收 %s 的文件 %q 校验失败（sha256 不匹配，文件可能在发送途中被修改）", peer, hdr.Name)
		writeResp(conn, Response{Error: "sha256 mismatch"})
		return
	}

	final, dup, err := resolvePath(dir, sanitizeName(hdr.Name), sum, hdr.Size)
	if err != nil {
		os.Remove(tmp)
		removeDirIfEmpty(dir)
		writeResp(conn, Response{Error: err.Error()})
		return
	}
	if dup {
		os.Remove(tmp)
	} else if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		removeDirIfEmpty(dir)
		writeResp(conn, Response{Error: "rename: " + err.Error()})
		return
	}
	writeResp(conn, Response{OK: true, Path: final, Duplicate: dup})
	if onFile != nil {
		onFile(hdr.Sender, final, dup, hdr.Bundle)
	}
}

// newTransferDir 在 receiveDir 下创建本次接收的专属子目录：
// `<20060102-150405>-<发送方名>`，同秒冲突时追加 -2、-3……
// 依赖 os.Mkdir 的排他性保证并发安全。
func newTransferDir(receiveDir, sender string) (string, error) {
	if sender == "" {
		sender = "peer"
	}
	base := time.Now().Format("20060102-150405") + "-" + sanitizeName(sender)
	dir := filepath.Join(receiveDir, base)
	for i := 2; ; i++ {
		err := os.Mkdir(dir, 0o755)
		if err == nil {
			return dir, nil
		}
		if !os.IsExist(err) {
			return "", err
		}
		dir = filepath.Join(receiveDir, fmt.Sprintf("%s-%d", base, i))
	}
}

// removeDirIfEmpty 清理接收失败后遗留的空子目录（非空时保留，便于排查）。
func removeDirIfEmpty(dir string) {
	os.Remove(dir)
}

func recvText(conn net.Conn, br *bufio.Reader, hdr Header, onText func(sender, text string), peer string) {
	if hdr.Size <= 0 || hdr.Size > maxTextBytes {
		writeResp(conn, Response{Error: "size out of range"})
		return
	}
	conn.SetDeadline(time.Now().Add(TimeoutFor(hdr.Size)))
	b, err := io.ReadAll(io.LimitReader(br, hdr.Size))
	if err != nil || int64(len(b)) != hdr.Size {
		log.Printf("接收 %s 的文本不完整: got %d/%d bytes", peer, len(b), hdr.Size)
		writeResp(conn, Response{Error: "transfer incomplete"})
		return
	}
	if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != hdr.SHA256 {
		log.Printf("接收 %s 的文本校验失败（sha256 不匹配）", peer)
		writeResp(conn, Response{Error: "sha256 mismatch"})
		return
	}
	writeResp(conn, Response{OK: true})
	if onText != nil {
		onText(hdr.Sender, string(b))
	}
}

// ErrUnauthorized 表示对端拒绝传输（未配对、配对已解除或 token 不一致）。
// 发送方收到此错误后应停止向该节点重试，提示用户在面板中重新配对。
var ErrUnauthorized = errors.New("对端拒绝传输（未配对或 token 不一致）：请在面板中重新配对，或将两侧 config.json 的 token 改成相同值")

// Send 连接对端并完成一次传输。
func Send(ip string, port int, hdr Header, payload func(w io.Writer) error, timeout time.Duration) (Response, error) {
	var resp Response
	addr := net.JoinHostPort(ip, fmt.Sprint(port))
	conn, err := net.DialTimeout("tcp4", addr, 5*time.Second)
	if err != nil {
		return resp, fmt.Errorf("连接 %s 失败: %w", addr, err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))

	bw := bufio.NewWriter(conn)
	b, _ := json.Marshal(hdr)
	if _, err := bw.Write(append(b, '\n')); err != nil {
		return resp, fmt.Errorf("发送协议头失败: %w", err)
	}
	if payload != nil {
		if err := payload(bw); err != nil {
			return resp, fmt.Errorf("发送数据失败: %w", err)
		}
	}
	if err := bw.Flush(); err != nil {
		return resp, fmt.Errorf("发送数据失败: %w", err)
	}

	line, err := bufio.NewReader(conn).ReadSlice('\n')
	if err != nil {
		if errors.Is(err, io.EOF) {
			return resp, errors.New("对端在应答前关闭了连接（对端可能传输超时、已退出或版本不兼容，请查看对端日志）")
		}
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return resp, errors.New("等待对端应答超时（链路过慢或对端无响应）")
		}
		return resp, fmt.Errorf("读取对端应答失败: %w", err)
	}
	if json.Unmarshal(bytes.TrimRight(line, "\r\n"), &resp) != nil {
		return resp, errors.New("bad response")
	}
	if !resp.OK && resp.Error != "" {
		if resp.Error == "unauthorized" {
			return resp, ErrUnauthorized
		}
		return resp, errors.New(resp.Error)
	}
	return resp, nil
}

// ---------- 辅助函数 ----------

func writeResp(conn net.Conn, resp Response) {
	b, _ := json.Marshal(resp)
	conn.Write(append(b, '\n'))
	// 应答后短暂排空客户端可能残留的多余数据（如文件中途变大导致超发），
	// 避免带未读数据关闭连接触发 RST 把应答吞掉，保证对端能收到 FIN+应答。
	conn.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	buf := make([]byte, 4096)
	for {
		if _, err := conn.Read(buf); err != nil {
			break
		}
	}
}

// sanitizeName 清理对端传来的文件名，只保留基础名与安全字符。
func sanitizeName(name string) string {
	name = filepath.Base(name)
	var b strings.Builder
	for _, r := range name {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(`<>:"|?*`, r) {
			b.WriteRune('_')
			continue
		}
		b.WriteRune(r)
	}
	s := strings.Trim(strings.TrimSpace(b.String()), ". ")
	if s == "" {
		s = "file"
	}
	return s
}

// resolvePath 决定接收文件在本次传输子目录内的最终路径；
// 同路径同内容（sha256 相同）返回 duplicate=true。子目录每次接收独占新建，
// 正常情况下不会走到重名分支（兜底逻辑仅防御异常竞态）。
func resolvePath(dir, name, sha string, size int64) (string, bool, error) {
	p := filepath.Join(dir, name)
	if _, err := os.Stat(p); err != nil {
		if os.IsNotExist(err) {
			return p, false, nil
		}
		return "", false, err
	}
	// 小文件做内容比对；大文件直接视为冲突改名
	if size <= 64<<20 && fileMatches(p, sha, size) {
		return p, true, nil
	}
	base, ext := splitName(name)
	for i := 1; i < 10000; i++ {
		cand := filepath.Join(dir, fmt.Sprintf("%s (%d)%s", base, i, ext))
		if _, err := os.Stat(cand); os.IsNotExist(err) {
			return cand, false, nil
		}
		if size <= 64<<20 && fileMatches(cand, sha, size) {
			return cand, true, nil
		}
	}
	return "", false, errors.New("too many name conflicts")
}

func fileMatches(p, sha string, size int64) bool {
	fi, err := os.Stat(p)
	if err != nil || fi.Size() != size {
		return false
	}
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false
	}
	return hex.EncodeToString(h.Sum(nil)) == sha
}

func splitName(name string) (base, ext string) {
	if ext = filepath.Ext(name); ext != "" {
		return strings.TrimSuffix(name, ext), ext
	}
	return name, ""
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
