package ui

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// lockedBuffer 并发安全缓冲，用于捕获渲染输出。
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// eoflessReader 永不返回数据的输入流（阻塞），模拟一个没有按键的终端。
type eoflessReader struct{ r io.Reader }

func (eoflessReader) Read(p []byte) (int, error) {
	time.Sleep(50 * time.Millisecond)
	return 0, io.EOF // 持续 EOF，驱动 bubbletea 输入循环空转
}

// TestProgramRendersToBuffer 用真实 bubbletea Program 管线验证渲染输出。
func TestProgramRendersToBuffer(t *testing.T) {
	ctl := newTestController()
	var out lockedBuffer

	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()

	p := tea.NewProgram(
		newModel(ctl),
		tea.WithInput(eoflessReader{r: io.NopCloser(strings.NewReader(""))}),
		tea.WithOutput(&out),
		tea.WithAltScreen(),
		tea.WithContext(ctx),
	)
	if _, err := p.Run(); err != nil && !strings.Contains(err.Error(), "context") {
		t.Fatalf("Program.Run: %v", err)
	}

	rendered := out.String()
	t.Logf("渲染字节数: %d", len(rendered))
	// 日志内容必须出现在内容区顶部附近（标签栏之后的前几行），而不是沉底
	logIdx := strings.Index(rendered, "copywhere 节点已启动")
	tabEnd := strings.Index(rendered, "3 文件记录 \x1b[0m")
	if logIdx < 0 || tabEnd < 0 {
		t.Fatalf("渲染输出缺少关键内容\n实际输出:\n%q", rendered)
	}
	gap := rendered[tabEnd:logIdx]
	if n := strings.Count(gap, "\n"); n > 4 {
		t.Fatalf("日志应从内容区顶部开始，实际距标签栏 %d 行\n实际输出:\n%q", n, rendered)
	}
	// 页脚必须仍在
	if !strings.Contains(rendered, "q 退出") {
		t.Fatalf("渲染输出缺少页脚\n实际输出:\n%q", rendered)
	}
	// View 不得输出 "\r\r\n"（每行末尾多余的 \r）
	if strings.Contains(rendered, "\r\r") {
		t.Fatalf("渲染输出含多余的回车符\n实际输出:\n%q", rendered)
	}
}
