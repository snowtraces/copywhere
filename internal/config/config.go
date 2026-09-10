package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Config 是 copywhere 的持久化配置。
// 注意：已无共享 token 字段——传输鉴权只认配对令牌（peers.json）。
type Config struct {
	NodeName      string `json:"node_name"`                   // 本节点显示名，默认主机名
	DiscoveryPort int    `json:"discovery_port"`              // UDP 发现端口
	TransferPort  int    `json:"transfer_port"`               // TCP 传输端口
	MaxAutoCopyMB int64  `json:"max_auto_copy_mb"`            // 剪贴板自动同步的容量阈值（MB），0 表示不限制
	ReceiveDir    string `json:"receive_dir"`                 // 接收文件保存目录
	AutoPaste     bool   `json:"auto_paste"`                  // 收到文件后自动写入本机剪贴板
	TextSync      bool   `json:"text_sync"`                   // 同步剪贴板文本
	AnnounceSec   int    `json:"announce_interval_sec"`       // 广播间隔（秒）
	PeerTTLSec    int    `json:"peer_ttl_sec"`                // 节点存活时长（秒）
	KVMEnabled    *bool  `json:"kvm_enabled,omitempty"`       // 鼠标键盘跨屏开关（缺省开启）
	KVMPort       int    `json:"kvm_port"`                    // KVM 监听端口
	KVMLeft       string `json:"kvm_left"`                    // 左边缘邻居节点名（光标推向左边缘时控制它）
	KVMRight      string `json:"kvm_right"`                   // 右边缘邻居节点名
	KVMEntryMon   *int   `json:"kvm_entry_monitor,omitempty"` // 被控入口显示器下标（缺省 -1=主显示器）
	WebPort       int    `json:"web_port"`                    // GUI 面板端口（gui 命令；0=默认，负数=随机）
	Path          string `json:"-"`                           // 配置文件实际路径（Load 时填充；信任库/面板地址等派生文件放在同目录）
}

// KVMEntryMonitorIdx 返回被控入口显示器下标（未配置 → -1 = 主显示器）。
func (c *Config) KVMEntryMonitorIdx() int {
	if c.KVMEntryMon == nil {
		return -1
	}
	return *c.KVMEntryMon
}

// Default 返回默认配置。
func Default() (*Config, error) {
	host, _ := os.Hostname()
	if host == "" {
		host = "node"
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return &Config{
		NodeName:      host,
		DiscoveryPort: 47830,
		TransferPort:  47831,
		MaxAutoCopyMB: 10,
		ReceiveDir:    filepath.Join(home, ".copywhere", "files"),
		AutoPaste:     true,
		TextSync:      true,
		AnnounceSec:   2,
		PeerTTLSec:    12,
		KVMPort:       47832,
		WebPort:       47890,
	}, nil
}

// DefaultPath 返回默认配置文件路径：~/.copywhere/config.json。
func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".copywhere", "config.json")
}

// Load 读取配置；文件不存在时自动生成默认配置并保存。
func Load(path string) (*Config, error) {
	if path == "" {
		path = DefaultPath()
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		cfg, derr := Default()
		if derr != nil {
			return nil, derr
		}
		cfg.Path = path
		if serr := cfg.Save(path); serr != nil {
			return nil, fmt.Errorf("写入默认配置失败: %w", serr)
		}
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析配置 %s 失败: %w", path, err)
	}
	cfg.Path = path
	if cfg.DiscoveryPort <= 0 {
		cfg.DiscoveryPort = 47830
	}
	if cfg.TransferPort <= 0 {
		cfg.TransferPort = 47831
	}
	if cfg.MaxAutoCopyMB < 0 {
		cfg.MaxAutoCopyMB = 10
	}
	if cfg.ReceiveDir == "" {
		home, _ := os.UserHomeDir()
		cfg.ReceiveDir = filepath.Join(home, ".copywhere", "files")
	}
	if cfg.AnnounceSec <= 0 {
		cfg.AnnounceSec = 2
	}
	if cfg.PeerTTLSec <= 0 {
		cfg.PeerTTLSec = 12
	}
	if cfg.KVMPort <= 0 {
		cfg.KVMPort = 47832
	}
	if cfg.WebPort < 0 {
		cfg.WebPort = 0 // 负数 = 随机端口
	} else if cfg.WebPort == 0 {
		cfg.WebPort = 47890
	}
	return &cfg, nil
}

// KVMOn 返回 KVM 是否启用（字段缺省即启用）。
func (c *Config) KVMOn() bool {
	return c.KVMEnabled == nil || *c.KVMEnabled
}

// Save 将配置写入指定路径（0600 权限）。
func (c *Config) Save(path string) error {
	if path == "" {
		path = DefaultPath()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// Threshold 返回自动同步阈值（字节）；0 表示不限制。
func (c *Config) Threshold() int64 {
	return c.MaxAutoCopyMB << 20
}

// RandomHex 返回 n 字节的十六进制随机串。
func RandomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
