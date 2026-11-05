package webui

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"copywhere/internal/config"
)

// ErrPanelNotRunning 表示没有可用的面板实例（gui 未运行，或面板已退出）。
// 资源管理器右键菜单的 helper 据此给出"请先启动 copywhere"这类具体提示。
var ErrPanelNotRunning = errors.New("copywhere 未运行")

// panelURLFile 是面板地址文件的文件名（与配置文件同目录）。
const panelURLFile = "panel_url.txt"

// PanelURLPath 返回面板地址文件路径：gui 启动时把带访问令牌的面板地址写入该文件
// （浏览器未自动打开时可手动查阅），右键菜单的 shell-send 也靠它找到运行中的实例。
func PanelURLPath(cfgPath string) string {
	if cfgPath == "" {
		cfgPath = config.DefaultPath()
	}
	return filepath.Join(filepath.Dir(cfgPath), panelURLFile)
}

// panelEndpoint 是从面板地址文件解析出的连接信息。
type panelEndpoint struct {
	base  string // 形如 http://127.0.0.1:47890/
	token string // 面板访问令牌（对应 ?key=）
}

// parsePanelURL 解析面板地址 "http://127.0.0.1:47890/?key=TOKEN"。
//
// 只接受回环地址：地址文件由本机用户可写，若被改写指向远端主机，
// 我们绝不把本地文件路径（用户的目录结构、文件名）发过去。
func parsePanelURL(raw string) (panelEndpoint, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return panelEndpoint{}, fmt.Errorf("面板地址无法解析: %q", raw)
	}
	switch u.Scheme {
	case "http":
	default:
		return panelEndpoint{}, fmt.Errorf("面板地址协议不受支持: %q", u.Scheme)
	}
	host := u.Hostname()
	if host != "127.0.0.1" && host != "localhost" && host != "::1" {
		return panelEndpoint{}, fmt.Errorf("面板地址不是本机回环地址: %q", host)
	}
	token := u.Query().Get("key")
	if token == "" {
		return panelEndpoint{}, errors.New("面板地址缺少访问令牌")
	}
	return panelEndpoint{base: "http://" + u.Host + "/", token: token}, nil
}

// ReadPanelURL 返回正在运行实例的面板地址（含访问令牌）。
//
// 供"第二次双击"这类场景使用：已有实例占着单实例锁时，新进程不再只丢一句
// 错误（用户看到的就是"双击没反应"），而是直接把已运行实例的面板打开。
// 没有运行中的实例时返回 ErrPanelNotRunning。
func ReadPanelURL(cfgPath string) (string, error) {
	ep, err := readPanelEndpoint(cfgPath)
	if err != nil {
		return "", err
	}
	return ep.base + "?key=" + ep.token, nil
}

// readPanelEndpoint 读取并解析面板地址文件。
func readPanelEndpoint(cfgPath string) (panelEndpoint, error) {
	data, err := os.ReadFile(PanelURLPath(cfgPath))
	if err != nil {
		return panelEndpoint{}, ErrPanelNotRunning
	}
	ep, err := parsePanelURL(string(data))
	if err != nil {
		return panelEndpoint{}, err
	}
	return ep, nil
}

// SendPathsViaPanel 把一批本地路径交给正在运行的实例发送（资源管理器右键菜单
// 的落地动作）。走运行中实例而不是自带一次发现+传输的理由：
//
//   - 发现端口是独占的——主实例占着 UDP 发现端口，新进程根本收不到公告；
//   - 运行中实例已有节点表与配对令牌，瞬间可发，无需等 4 秒扫描；
//   - 发送过程与结果记入面板工作台/日志，用户能看到"发出去了什么"。
//
// 返回的错误已可直接展示给用户（未运行、无在线节点、均未配对等）。
func SendPathsViaPanel(cfgPath string, paths []string) error {
	if len(paths) == 0 {
		return errors.New("没有要发送的文件")
	}
	ep, err := readPanelEndpoint(cfgPath)
	if err != nil {
		return err
	}
	return postPaths(ep, paths)
}

// postPaths 把路径 POST 给面板：200 表示已受理（真正发送由实例合并后进行），
// 其他状态码的响应体就是给人看的中文原因。
func postPaths(ep panelEndpoint, paths []string) error {
	body, err := json.Marshal(map[string]any{"paths": paths})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, ep.base+"api/send-paths?key="+url.QueryEscape(ep.token),
		bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	// 面板侧只做登记（发送异步进行），正常应在毫秒级返回；超时只可能是实例卡死。
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%w（连接面板失败: %v）", ErrPanelNotRunning, err)
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode == http.StatusNotFound {
		// 旧版本实例不认识 /api/send-paths：升级 exe 后必须重启进程才生效
		return errors.New("正在运行的 copywhere 版本不支持右键发送：请先退出（托盘右键「退出」）再重新启动 copywhere")
	}
	if resp.StatusCode != http.StatusOK {
		if text := strings.TrimSpace(string(msg)); text != "" {
			return errors.New(text)
		}
		return fmt.Errorf("面板返回状态 %d", resp.StatusCode)
	}
	return nil
}
