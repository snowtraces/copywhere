package webui

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParsePanelURL(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		base    string
		token   string
		wantErr bool
	}{
		{name: "回环地址 + 令牌", raw: "http://127.0.0.1:47890/?key=abc123",
			base: "http://127.0.0.1:47890/", token: "abc123"},
		{name: "localhost", raw: "http://localhost:47890/?key=k",
			base: "http://localhost:47890/", token: "k"},
		{name: "缺令牌", raw: "http://127.0.0.1:47890/", wantErr: true},
		// 地址文件本机可写：指向远端时绝不能把本地路径发出去
		{name: "非回环地址", raw: "http://10.0.0.9:47890/?key=abc", wantErr: true},
		{name: "非 http 协议", raw: "https://127.0.0.1:47890/?key=abc", wantErr: true},
		{name: "无法解析", raw: "这不是地址", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ep, err := parsePanelURL(c.raw)
			if c.wantErr {
				if err == nil {
					t.Fatalf("应报错，实际解析为 %+v", ep)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if ep.base != c.base || ep.token != c.token {
				t.Fatalf("期望 %q/%q，实际 %q/%q", c.base, c.token, ep.base, ep.token)
			}
		})
	}
}

// TestSendPathsViaPanelNotRunning 覆盖"没运行 gui"的常见情形：地址文件不存在，
// helper 必须拿到可判定的 ErrPanelNotRunning（据此提示用户先启动）。
func TestSendPathsViaPanelNotRunning(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	err := SendPathsViaPanel(cfgPath, []string{`C:\x\a.txt`})
	if !errors.Is(err, ErrPanelNotRunning) {
		t.Fatalf("应返回 ErrPanelNotRunning，实际 %v", err)
	}
}

// TestSendPathsViaPanel 走完整 helper 路径：读地址文件 → POST → 交给运行中实例。
func TestSendPathsViaPanel(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/send-paths" {
			t.Errorf("路径应为 /api/send-paths，实际 %s", r.URL.Path)
		}
		if r.URL.Query().Get("key") != "tok" {
			http.Error(w, "缺少访问令牌", http.StatusUnauthorized)
			return
		}
		var in struct {
			Paths []string `json:"paths"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		got = in.Paths
		writeJSON(w, map[string]any{"ok": true, "count": len(in.Paths)})
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	urlFile := PanelURLPath(cfgPath)
	// 面板写入的地址文件格式（NewServer 生成的 URL）
	if err := os.WriteFile(urlFile, []byte(srv.URL+"/?key=tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	paths := []string{`C:\a\1.txt`, `C:\a\2.txt`}
	if err := SendPathsViaPanel(cfgPath, paths); err != nil {
		t.Fatalf("SendPathsViaPanel 失败: %v", err)
	}
	if len(got) != 2 || got[0] != paths[0] || got[1] != paths[1] {
		t.Fatalf("实例应收到 %v，实际 %v", paths, got)
	}
}

// TestPostPathsSurfacesServerReason 失败时错误文本必须是面板给出的原因，
// 这样 helper 的弹窗能直接告诉用户"未发现在线节点""均未配对"。
func TestPostPathsSurfacesServerReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "在线节点均未配对（或鉴权被拒）：请先在面板中完成配对", http.StatusConflict)
	}))
	defer srv.Close()

	err := postPaths(panelEndpoint{base: srv.URL + "/", token: "tok"}, []string{`C:\a.txt`})
	if err == nil || err.Error() != "在线节点均未配对（或鉴权被拒）：请先在面板中完成配对" {
		t.Fatalf("应原样返回面板原因，实际 %v", err)
	}
}

// TestPostPathsReportsOldInstance 升级场景：旧实例没有 /api/send-paths（404），
// 提示必须指向"重启 copywhere"，而不是含糊的 404。
func TestPostPathsReportsOldInstance(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	err := postPaths(panelEndpoint{base: srv.URL + "/", token: "tok"}, []string{`C:\a.txt`})
	if err == nil || !strings.Contains(err.Error(), "重新启动 copywhere") {
		t.Fatalf("404 应提示重启实例，实际 %v", err)
	}
}

// TestPanelURLPath 保证 helper 与 gui 使用同一处地址文件（配置同目录）。
func TestPanelURLPath(t *testing.T) {
	dir := t.TempDir()
	if got, want := PanelURLPath(filepath.Join(dir, "config.json")), filepath.Join(dir, panelURLFile); got != want {
		t.Fatalf("期望 %q，实际 %q", want, got)
	}
}

// 第二次双击时把已运行实例的面板地址读出来复用。
func TestReadPanelURL(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	// 未运行：应返回 ErrPanelNotRunning，而不是莫名其妙的路径错误
	if _, err := ReadPanelURL(cfgPath); !errors.Is(err, ErrPanelNotRunning) {
		t.Fatalf("没有地址文件时应返回 ErrPanelNotRunning，实际 %v", err)
	}

	want := "http://127.0.0.1:47890/?key=abc123"
	if err := os.WriteFile(PanelURLPath(cfgPath), []byte(want+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadPanelURL(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("期望 %q，实际 %q", want, got)
	}

	// 被改写成远端地址时必须拒绝（不把本地路径发给外部主机）
	os.WriteFile(PanelURLPath(cfgPath), []byte("http://192.168.1.9:47890/?key=abc123"), 0o600)
	if _, err := ReadPanelURL(cfgPath); err == nil {
		t.Fatal("远端地址应被拒绝")
	}
}
