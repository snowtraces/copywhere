package wailsui

import (
	"os"
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
		"loadState", "loadPeers", "loadRecords", "loadLogs", "loadPendingPair",
		"applyState", "renderSub", "renderPeers", "addRecord", "renderLayout",
		"bindStaticUI", "bindWailsEvents", "toast",
		"loadPersistedFeed", "slimFeed", "schedulePersistFeed", "clearPersistedFeed",
		"pickFiles", "pasteFilesFromClipboard", "sendText",
		"boot", "renderLogs", "updateLogBadge", "openLogs", "closeLogs", "togglePause",
	}
	for _, name := range critical {
		defined := regexp.MustCompile(`(?m)^(?:async\s+)?function\s+` + regexp.QuoteMeta(name) + `\s*\(`).MatchString(js)
		arrow := regexp.MustCompile(`(?m)^const\s+` + regexp.QuoteMeta(name) + `\s*=`).MatchString(js)
		if !defined && !arrow {
			t.Errorf("前端缺少函数 %s（被误删？）", name)
		}
	}
}
