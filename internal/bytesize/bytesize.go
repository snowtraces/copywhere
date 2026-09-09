// Package bytesize 提供人类可读的字节数格式化。
package bytesize

import "fmt"

// Human 将字节数格式化为 "1.5 MB" 之类的可读形式。
func Human(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
