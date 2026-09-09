package ui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"copywhere/internal/discovery"
)

func newTestController() *Controller {
	ctl := NewController(func() []discovery.Peer {
		return []discovery.Peer{{
			ID: "p1", Name: "RANDOM",
			IPs:     []string{"192.168.0.93", "10.147.17.171"},
			TCPPort: 47831, LastSeen: time.Now(),
		}}
	}, 12*time.Second, "MYPC", "192.168.0.108")
	ctl.Hub.Write([]byte("19:00:00 copywhere 节点已启动\n"))
	ctl.AddFile(FileRecord{
		Time: time.Now(), In: true,
		Peer: "RANDOM (192.168.0.93)", Name: "a.txt", Size: 1024, Status: "已接收",
	})
	return ctl
}

func TestViewShowsAllTabsContent(t *testing.T) {
	ctl := newTestController()
	mm := step(newModel(ctl), tea.WindowSizeMsg{Width: 100, Height: 30})
	// 文件记录经 channel 投递，测试环境没有运行中的 Program 消费，直接注入
	mm.files = append(mm.files, FileRecord{
		Time: time.Now(), In: true,
		Peer: "RANDOM (192.168.0.93)", Name: "a.txt", Size: 1024, Status: "已接收",
	})

	// 日志页
	v := mm.View()
	for _, want := range []string{"日志", "在线节点", "文件记录", "copywhere 节点已启动"} {
		if !strings.Contains(v, want) {
			t.Fatalf("日志页缺少 %q\n视图:\n%s", want, v)
		}
	}

	// → 节点页
	mm = step(mm, tea.KeyMsg{Type: tea.KeyRight})
	v = mm.View()
	for _, want := range []string{"[本机]MYPC(192.168.0.108)", "[远程]RANDOM(192.168.0.93)", "10.147.17.171"} {
		if !strings.Contains(v, want) {
			t.Fatalf("节点页缺少 %q\n视图:\n%s", want, v)
		}
	}

	// ← 回日志页，→→ 到文件页
	mm = step(mm, tea.KeyMsg{Type: tea.KeyLeft})
	mm = step(mm, tea.KeyMsg{Type: tea.KeyRight})
	mm = step(mm, tea.KeyMsg{Type: tea.KeyRight})
	v = mm.View()
	if !strings.Contains(v, "a.txt") {
		t.Fatalf("文件页缺少记录\n视图:\n%s", v)
	}
}

// 回归：终端尺寸上报为 0 时（某些 console 环境），正文不得被截成空白。
func TestZeroWindowSizeStillRenders(t *testing.T) {
	mm := step(newModel(newTestController()), tea.WindowSizeMsg{Width: 0, Height: 0})
	v := mm.View()
	if !strings.Contains(v, "copywhere 节点已启动") {
		t.Fatalf("尺寸为 0 时日志页丢失内容\n视图:\n%s", v)
	}
	// 切到节点页：[本机] 行必须可见
	mm = step(mm, tea.KeyMsg{Type: tea.KeyRight})
	v = mm.View()
	if !strings.Contains(v, "[本机]MYPC(192.168.0.108)") {
		t.Fatalf("尺寸为 0 时节点页丢失 [本机] 行\n视图:\n%s", v)
	}
}

// step 驱动一次 Update 并取回 model。
func step(m model, msg tea.Msg) model {
	next, _ := m.Update(msg)
	return next.(model)
}
