package clip

// Rich 是带格式的剪贴板内容：纯文本兜底 + 可选的 HTML / RTF 富格式。
//
// 借鉴 MouseWithoutBorders 的富文本同步思路（其只同步 RTF）：Windows 应用
// 复制富文本时通常会同时放入 CF_UNICODETEXT / "HTML Format" / "Rich Text Format"，
// 只搬运纯文本会让 Word/网页/PPT 的内容到对端变成"无格式砖块"。copywhere
// 把三种格式一起搬运，对端 Ctrl+V 时由目标应用自行挑选，粘贴效果与本机一致。
//
// HTML 字段原样搬运 CF_HTML 数据块（其中 StartFragment/EndFragment 偏移
// 指向块内自身，跨机器整体复制后仍然有效，无需重写）。
type Rich struct {
	Text string `json:"text"`
	HTML []byte `json:"html,omitempty"`
	RTF  []byte `json:"rtf,omitempty"`
}

// HasFormats 返回除纯文本外是否还带有富格式。
func (r Rich) HasFormats() bool { return len(r.HTML) > 0 || len(r.RTF) > 0 }

// Size 返回各格式负载总字节数（含文本），供阈值判断与日志展示。
func (r Rich) Size() int64 {
	return int64(len(r.Text)) + int64(len(r.HTML)) + int64(len(r.RTF))
}
