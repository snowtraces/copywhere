// Package transport 实现节点间的 TCP 传输协议（服务端 + 客户端）。
//
// 协议：单行 JSON 头（\n 结尾）+ 原始负载 + 单行 JSON 应答。
// 文件先落盘为临时文件，校验 sha256 后再改名；同名同内容视为重复。
package transport

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
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
	V      int    `json:"v"`      // 协议版本
	Token  string `json:"token"`  // 鉴权令牌
	Type   string `json:"type"`   // "file" | "text"
	Name   string `json:"name"`   // 文件名（text 时为 "text"）
	Size   int64  `json:"size"`   // 负载字节数
	SHA256 string `json:"sha256"` // 负载 sha256
	Sender string `json:"sender"` // 发送方节点名
}

// Response 是接收方的 JSON 应答。
type Response struct {
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
	Path      string `json:"path,omitempty"`      // 接收后的最终路径
	Duplicate bool   `json:"duplicate,omitempty"` // 内容已存在
}

// Server 启动 TCP 传输服务，直到 ctx 结束。
func Server(ctx context.Context, cfg *config.Config,
	onFile func(sender, path string, dup bool),
	onText func(sender, text string)) error {

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
		go handle(conn, cfg, onFile, onText)
	}
}

func handle(conn net.Conn, cfg *config.Config,
	onFile func(sender, path string, dup bool),
	onText func(sender, text string)) {

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
	if hdr.V != 1 || subtle.ConstantTimeCompare([]byte(hdr.Token), []byte(cfg.Token)) != 1 {
		log.Printf("拒绝 %s 的连接（token 不匹配，请检查两侧 config.json 的 token 是否一致）", peer)
		writeResp(conn, Response{Error: "unauthorized"})
		return
	}
	switch hdr.Type {
	case "file":
		recvFile(conn, br, hdr, cfg, onFile, peer)
	case "text":
		recvText(conn, br, hdr, onText, peer)
	default:
		writeResp(conn, Response{Error: "unknown type"})
	}
}

func recvFile(conn net.Conn, br *bufio.Reader, hdr Header, cfg *config.Config,
	onFile func(sender, path string, dup bool), peer string) {

	if hdr.Size < 0 || hdr.Size > maxFileBytes {
		writeResp(conn, Response{Error: "size out of range"})
		return
	}
	dir := cfg.ReceiveDir
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeResp(conn, Response{Error: "mkdir: " + err.Error()})
		return
	}
	conn.SetDeadline(time.Now().Add(TimeoutFor(hdr.Size)))

	tmp := filepath.Join(dir, ".cw-partial-"+randHex(6))
	out, err := os.Create(tmp)
	if err != nil {
		writeResp(conn, Response{Error: "create temp: " + err.Error()})
		return
	}
	h := sha256.New()
	n, cerr := io.Copy(io.MultiWriter(out, h), io.LimitReader(br, hdr.Size))
	out.Close()

	if cerr != nil || n != hdr.Size {
		os.Remove(tmp)
		log.Printf("接收 %s 的文件 %q 不完整: got %d/%d bytes, err=%v（多为链路过慢触发超时或对端中断）",
			peer, hdr.Name, n, hdr.Size, cerr)
		writeResp(conn, Response{Error: fmt.Sprintf("transfer incomplete: got %d/%d bytes", n, hdr.Size)})
		return
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if sum != hdr.SHA256 {
		os.Remove(tmp)
		log.Printf("接收 %s 的文件 %q 校验失败（sha256 不匹配，文件可能在发送途中被修改）", peer, hdr.Name)
		writeResp(conn, Response{Error: "sha256 mismatch"})
		return
	}

	final, dup, err := resolvePath(dir, sanitizeName(hdr.Name), sum, hdr.Size)
	if err != nil {
		os.Remove(tmp)
		writeResp(conn, Response{Error: err.Error()})
		return
	}
	if dup {
		os.Remove(tmp)
	} else if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		writeResp(conn, Response{Error: "rename: " + err.Error()})
		return
	}
	writeResp(conn, Response{OK: true, Path: final, Duplicate: dup})
	if onFile != nil {
		onFile(hdr.Sender, final, dup)
	}
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

// ErrUnauthorized 表示对端因 token 不一致拒绝了传输。
// 发送方收到此错误后应停止向该节点重试（改 token 需对端重启才生效）。
var ErrUnauthorized = errors.New("对端拒绝传输（token 不一致）：请把两侧 config.json 的 token 改成相同值后重启对端")

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

// resolvePath 决定接收文件的最终路径；同路径同内容（sha256 相同）返回 duplicate=true。
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
