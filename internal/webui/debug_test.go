package webui

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestDebugEndpointsAuth 运行时诊断端点必须与其他面板端点同级鉴权：
// 未带令牌一律 401。
func TestDebugEndpointsAuth(t *testing.T) {
	srv, _ := newTestServer(t)
	base, _ := splitURL(srv)
	resp, err := http.Get(base + "api/debug")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/api/debug 无令牌应 401，实际 %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("POST", base+"api/debug/stack", nil)
	r2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/api/debug/stack 无令牌应 401，实际 %d", r2.StatusCode)
	}
}

// TestDebugSnapshotAndStack 带令牌访问：JSON 快照结构完整，
// 栈转储成功且 2 秒限流生效（第二次返回 ok=false）。
func TestDebugSnapshotAndStack(t *testing.T) {
	srv, _ := newTestServer(t)
	base, key := splitURL(srv)

	resp, err := http.Get(base + "api/debug?key=" + key)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("带令牌应 200，实际 %d", resp.StatusCode)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("JSON 解析失败: %v", err)
	}
	for _, k := range []string{"version", "snapshot", "peers", "state", "listen"} {
		if _, ok := payload[k]; !ok {
			t.Errorf("快照缺少字段 %q", k)
		}
	}
	// HTML 视图（浏览器直接访问）：转储按钮必须用绝对路径（相对 /api/debug
	// 会错误解析成 /api/api/debug/stack，历史上导致按钮必然 404）。
	req, _ := http.NewRequest("GET", base+"api/debug?key="+key, nil)
	req.Header.Set("Accept", "text/html")
	hr, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	hb, _ := io.ReadAll(hr.Body)
	hr.Body.Close()
	if !strings.Contains(string(hb), `fetch('/api/debug/stack'`) {
		t.Fatal("诊断页转储按钮必须使用绝对路径 /api/debug/stack")
	}

	// 栈转储 + 限流
	preq, _ := http.NewRequest("POST", base+"api/debug/stack?key="+key, nil)
	pr, err := http.DefaultClient.Do(preq)
	if err != nil {
		t.Fatal(err)
	}
	pb, _ := io.ReadAll(pr.Body)
	pr.Body.Close()
	var first map[string]any
	if json.Unmarshal(pb, &first) != nil || first["ok"] != true {
		t.Fatalf("首次转储应成功: %s", pb)
	}
	pr2, err := http.DefaultClient.Do(preq)
	if err != nil {
		t.Fatal(err)
	}
	pb2, _ := io.ReadAll(pr2.Body)
	pr2.Body.Close()
	var second map[string]any
	if json.Unmarshal(pb2, &second) != nil || second["ok"] != false {
		t.Fatalf("2 秒内重复转储应被限流: %s", pb2)
	}
}
