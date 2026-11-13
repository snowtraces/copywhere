package wailsui

import (
	"os"
	"reflect"
	"regexp"
	"sort"
	"testing"
)

// 前端没有构建期检查：node --check 只查语法，函数被误删/元素 id 打错都要等运行期
// 才炸（曾出现启动即 "loadLogs is not defined"）。这里做两条静态护栏：
//  1. app.js 里所有 $("id") 引用的元素必须在 index.html 中存在；
//  2. 关键函数（尤其 boot 路径上的加载函数）必须仍有定义。
func TestFrontendWiring(t *testing.T) {
	jsRaw, err := os.ReadFile("frontend/app.js")
	if err != nil {
		t.Fatalf("读取 app.js: %v", err)
	}
	htmlRaw, err := os.ReadFile("frontend/index.html")
	if err != nil {
		t.Fatalf("读取 index.html: %v", err)
	}
	js, html := string(jsRaw), string(htmlRaw)

	refRe := regexp.MustCompile(`\$\(\s*"([^"]+)"\s*\)`)
	refs := map[string]bool{}
	for _, m := range refRe.FindAllStringSubmatch(js, -1) {
		refs[m[1]] = true
	}
	idRe := regexp.MustCompile(`id="([^"]+)"`)
	ids := map[string]bool{}
	for _, m := range idRe.FindAllStringSubmatch(html, -1) {
		ids[m[1]] = true
	}
	var missing []string
	for id := range refs {
		if !ids[id] {
			missing = append(missing, id)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("app.js 引用了 index.html 里不存在的元素 id: %v", missing)
	}

	// 这些函数分布在启动、时间线、日志抽屉与输入框交互的关键路径上。
	critical := []string{
		"loadState", "loadPeers", "loadRecords", "loadLogs", "loadPendingPair", "loadUiState",
		"applyState", "renderSub", "renderPeers", "addRecord", "renderLayout",
		"bindStaticUI", "bindWailsEvents", "toast",
		"loadPersistedFeed", "slimFeed", "slimFav", "persistFavorites", "schedulePersistFeed", "clearPersistedFeed",
		"pickFiles", "pasteFilesFromClipboard", "sendText",
		"boot", "renderLogs", "updateLogBadge", "openLogs", "closeLogs", "togglePause",
		"applyUiState", "renderMini", "renderMiniPeers", "renderMiniFeed", "renderMiniXfers",
		"setUiMode", "toggleUiMode", "toggleMiniPin", "collapseMini", "expandMini",
		"bindMiniDockDrag", "expandFromDock",
		"sendMiniText", "handleComposerPaste", "hideWindow",
		"isTextRecord", "recordPath", "previewImage", "copyRecordText",
		"miniTextBubble", "miniImageBubble", "miniFileBubble",
		"starSvg", "updateRecordRowFav", "updateMiniRowFav", "toggleRecordFav",
		"applyFeedFilter", "setFeedFilter", "updateFavBadges", "updateMiniFavBtn", "toggleMiniFavFilter",
	}
	for _, name := range critical {
		defined := regexp.MustCompile(`(?m)^(?:async\s+)?function\s+` + regexp.QuoteMeta(name) + `\s*\(`).MatchString(js)
		arrow := regexp.MustCompile(`(?m)^const\s+` + regexp.QuoteMeta(name) + `\s*=`).MatchString(js)
		if !defined && !arrow {
			t.Errorf("前端缺少函数 %s（被误删？）", name)
		}
	}
}

// TestFrontendBoundMethodsExist 检查 app.js 里每个 call("Method") 都能在 Service
// 上找到对应方法。WebView 里绑定失败只会表现为"点了没反应"，日志里也看不到；
// 之前 revert 误删 Settings/Paired/Info/Logs 时设置页与日志抽屉就是这样静默失效的。
func TestFrontendBoundMethodsExist(t *testing.T) {
	jsRaw, err := os.ReadFile("frontend/app.js")
	if err != nil {
		t.Fatalf("读取 app.js: %v", err)
	}
	callRe := regexp.MustCompile(`call\(\s*"([A-Za-z_][A-Za-z0-9_]*)"`)
	called := map[string]bool{}
	for _, m := range callRe.FindAllStringSubmatch(string(jsRaw), -1) {
		called[m[1]] = true
	}
	if len(called) == 0 {
		t.Fatal("没有从 app.js 解析到任何 call(\"Method\")，正则或前端写法变了？")
	}

	svc := reflect.TypeOf(&Service{})
	var missing []string
	for name := range called {
		if _, ok := svc.MethodByName(name); !ok {
			missing = append(missing, name)
		}
	}
	// 前端图片预览的两个方法各写一条字面量调用，正则能覆盖；这里再显式列一遍，
	// 保证以后有人把它们合并回变量拼接时，至少这一层还守着。
	for _, name := range []string{"ImagePreview", "LocalImagePreview"} {
		if _, ok := svc.MethodByName(name); !ok {
			missing = append(missing, name+"（图片预览）")
		}
		if !called[name] {
			t.Errorf("app.js 里找不到 call(%q)：图片气泡会静默退化成文件卡片", name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("app.js 调用了 Service 上不存在的方法: %v", missing)
	}
}
