//go:build !windows

package clip

import "errors"

// 非 Windows 平台的占位实现：copywhere 的剪贴板功能仅支持 Windows。

// ErrNoFiles 表示剪贴板当前没有文件列表。
var ErrNoFiles = errors.New("剪贴板中没有文件列表 (CF_HDROP)")

// ErrNoText 表示剪贴板当前没有文本。
var ErrNoText = errors.New("剪贴板中没有文本 (CF_UNICODETEXT)")

func Seq() uint32 { return 0 }

func ReadFiles() ([]string, error) {
	return nil, errors.New("copywhere 的剪贴板功能仅支持 Windows")
}

func ReadText() (string, error) {
	return "", errors.New("copywhere 的剪贴板功能仅支持 Windows")
}

func SetFiles(paths []string) error {
	return errors.New("copywhere 的剪贴板功能仅支持 Windows")
}

func SetText(s string) error {
	return errors.New("copywhere 的剪贴板功能仅支持 Windows")
}
