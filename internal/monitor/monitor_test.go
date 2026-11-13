package monitor

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"log"
	"strings"
	"testing"

	"copywhere/internal/clip"
)

type mockSender struct {
	sentFiles [][]string
	sentRich  []clip.Rich
	sentImg   [][]byte
}

func (m *mockSender) SendFiles(paths []string, total int64) {
	m.sentFiles = append(m.sentFiles, paths)
}
func (m *mockSender) SendText(text string) {}
func (m *mockSender) SendRich(r clip.Rich) {
	m.sentRich = append(m.sentRich, r)
}
func (m *mockSender) SendImage(png []byte) {
	m.sentImg = append(m.sentImg, png)
}

func TestConsecutiveDeduplicationText(t *testing.T) {
	var buf bytes.Buffer
	origWriter := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(origWriter)

	sender := &mockSender{}
	d := newDetector(Guard{}, func() int64 { return 0 }, func() bool { return true }, sender)

	var currentSeq uint32 = 100
	var currentRich clip.Rich

	d.getSeq = func() uint32 { return currentSeq }
	d.readFiles = func() ([]string, error) { return nil, clip.ErrNoFiles }
	d.readRich = func() (clip.Rich, error) { return currentRich, nil }
	d.readImage = func() ([]byte, error) { return nil, clip.ErrNoImage }

	// 1. 复制文本 "msg1"
	currentSeq++
	currentRich = clip.Rich{Text: "msg1"}
	d.detect()
	if len(sender.sentRich) != 1 || sender.sentRich[0].Text != "msg1" {
		t.Fatalf("第一次复制应推送，实际 sentRich: %v", sender.sentRich)
	}

	// 2. 连续复制相同文本 "msg1"（即使产生新 seq）
	currentSeq++
	d.detect()
	if len(sender.sentRich) != 1 {
		t.Fatalf("连续复制相同内容应跳过，实际发送了: %d 次", len(sender.sentRich))
	}
	expectedMD5 := hex.EncodeToString(func() []byte {
		h := md5.Sum([]byte("msg1"))
		return h[:]
	}())
	if !strings.Contains(buf.String(), "剪贴板文本连续重复，已跳过") || !strings.Contains(buf.String(), expectedMD5) {
		t.Fatalf("未在日志中发现连续重复记录，日志内容: %s", buf.String())
	}

	// 3. 复制文本 "msg2"（新消息）
	currentSeq++
	currentRich = clip.Rich{Text: "msg2"}
	d.detect()
	if len(sender.sentRich) != 2 || sender.sentRich[1].Text != "msg2" {
		t.Fatalf("新消息应推送，实际 sentRich: %v", sender.sentRich)
	}

	// 4. 再次复制文本 "msg1"（跨消息复制，必须继续推送）
	currentSeq++
	currentRich = clip.Rich{Text: "msg1"}
	d.detect()
	if len(sender.sentRich) != 3 || sender.sentRich[2].Text != "msg1" {
		t.Fatalf("跨消息复制应继续推送，实际 sentRich: %v", sender.sentRich)
	}
}

func TestCrossTypeDeduplication(t *testing.T) {
	sender := &mockSender{}
	d := newDetector(Guard{}, func() int64 { return 0 }, func() bool { return true }, sender)

	var currentSeq uint32 = 10
	var currentRich clip.Rich
	var currentImg []byte
	var currentFiles []string

	d.getSeq = func() uint32 { return currentSeq }
	d.readFiles = func() ([]string, error) {
		if len(currentFiles) > 0 {
			return currentFiles, nil
		}
		return nil, clip.ErrNoFiles
	}
	d.calcFP = func(paths []string) (string, int64, error) {
		return strings.Join(paths, "|"), 100, nil
	}
	d.readRich = func() (clip.Rich, error) {
		if currentRich.Text != "" {
			return currentRich, nil
		}
		return clip.Rich{}, clip.ErrNoText
	}
	d.readImage = func() ([]byte, error) {
		if len(currentImg) > 0 {
			return currentImg, nil
		}
		return nil, clip.ErrNoImage
	}

	// 1. 复制文本 "hello"
	currentSeq++
	currentRich = clip.Rich{Text: "hello"}
	d.detect()
	if len(sender.sentRich) != 1 {
		t.Fatalf("文本应发送")
	}

	// 2. 复制截图
	currentSeq++
	currentRich = clip.Rich{}
	currentImg = []byte("png-data-1")
	d.detect()
	if len(sender.sentImg) != 1 {
		t.Fatalf("截图应发送")
	}

	// 3. 连续复制相同截图：跳过
	currentSeq++
	d.detect()
	if len(sender.sentImg) != 1 {
		t.Fatalf("连续相同截图应被跳过")
	}

	// 4. 再次复制文本 "hello"（跨类型后复制，必须继续推送）
	currentSeq++
	currentImg = nil
	currentRich = clip.Rich{Text: "hello"}
	d.detect()
	if len(sender.sentRich) != 2 {
		t.Fatalf("跨类型后重新复制原文本应继续推送")
	}

	// 5. 复制文件列表
	currentSeq++
	currentRich = clip.Rich{}
	currentFiles = []string{"C:\\test1.txt"}
	d.detect()
	if len(sender.sentFiles) != 1 {
		t.Fatalf("文件列表应发送")
	}

	// 6. 连续复制相同文件列表：跳过
	currentSeq++
	d.detect()
	if len(sender.sentFiles) != 1 {
		t.Fatalf("连续复制相同文件列表应跳过")
	}
}

func TestRichTextFormatDistinction(t *testing.T) {
	sender := &mockSender{}
	d := newDetector(Guard{}, func() int64 { return 0 }, func() bool { return true }, sender)

	var currentSeq uint32 = 100
	var currentRich clip.Rich

	d.getSeq = func() uint32 { return currentSeq }
	d.readFiles = func() ([]string, error) { return nil, clip.ErrNoFiles }
	d.readRich = func() (clip.Rich, error) { return currentRich, nil }
	d.readImage = func() ([]byte, error) { return nil, clip.ErrNoImage }

	// 1. 复制纯文本 "text"
	currentSeq++
	currentRich = clip.Rich{Text: "text"}
	d.detect()
	if len(sender.sentRich) != 1 {
		t.Fatalf("纯文本应发送")
	}

	// 2. 复制相同文字但带有 HTML 的富文本（格式不同，视为新内容，不应被去重拦截）
	currentSeq++
	currentRich = clip.Rich{Text: "text", HTML: []byte("<b>text</b>")}
	d.detect()
	if len(sender.sentRich) != 2 {
		t.Fatalf("格式不同的文本应作为新消息发送")
	}

	// 3. 连续复制相同富文本 -> 连续重复跳过
	currentSeq++
	d.detect()
	if len(sender.sentRich) != 2 {
		t.Fatalf("连续复制相同富文本应跳过")
	}
}

func TestThresholdAndConsecutiveDeduplication(t *testing.T) {
	var buf bytes.Buffer
	origWriter := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(origWriter)

	sender := &mockSender{}
	// 阈值限制为 50 字节
	d := newDetector(Guard{}, func() int64 { return 50 }, func() bool { return true }, sender)

	var currentSeq uint32 = 200
	d.getSeq = func() uint32 { return currentSeq }
	d.readFiles = func() ([]string, error) { return []string{"big.bin"}, nil }
	d.calcFP = func(paths []string) (string, int64, error) { return "big.bin|100", 100, nil }
	d.readRich = func() (clip.Rich, error) { return clip.Rich{}, clip.ErrNoText }
	d.readImage = func() ([]byte, error) { return nil, clip.ErrNoImage }

	// 1. 首次检测：超阈值（100 > 50），记录超限跳过日志，不发送
	currentSeq++
	d.detect()
	if len(sender.sentFiles) != 0 {
		t.Fatalf("超限文件不应发送")
	}
	if !strings.Contains(buf.String(), "超过自动同步阈值") {
		t.Fatalf("首次超限应打印超限日志")
	}

	buf.Reset()
	// 2. 再次连续检测相同超限文件：判定为连续重复跳过
	currentSeq++
	d.detect()
	if len(sender.sentFiles) != 0 {
		t.Fatalf("超限文件不应发送")
	}
	if !strings.Contains(buf.String(), "剪贴板文件连续重复，已跳过") {
		t.Fatalf("连续复制应打印连续重复跳过日志")
	}
}
