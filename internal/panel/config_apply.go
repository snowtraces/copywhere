package panel

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"copywhere/internal/config"
)

// ApplyConfig 应用配置修改，是桌面窗口设置页的唯一入口。
//
// 顺序：校验并写入内存 → 落盘 → 热更新（KVM 开关 / 触控板手势 / 邻居同步）。
// 校验失败时返回可读错误，已通过校验的字段会留在内存里（不落盘）；
// 该行为与旧网页窗口一致，避免用户在修一个字段时丢掉其它已填内容。
func ApplyConfig(core Core, cfg *config.Config, cfgPath string, in map[string]any) error {
	var errs []string

	applyStr := func(key string, dst *string) {
		if v, ok := in[key]; ok {
			sv, ok := v.(string)
			if !ok || strings.TrimSpace(sv) == "" {
				errs = append(errs, key+" 需要非空字符串")
				return
			}
			*dst = sv
		}
	}
	applyStr("receive_dir", &cfg.ReceiveDir)

	nodeNameChanged := false
	if v, ok := in["node_name"]; ok {
		if sv, ok := v.(string); ok && strings.TrimSpace(sv) != "" {
			if cfg.NodeName != sv {
				nodeNameChanged = true
			}
			cfg.NodeName = sv
		}
	}
	if v, ok := in["max_auto_copy_mb"]; ok {
		if fv, ok := v.(float64); ok && fv >= 0 {
			cfg.MaxAutoCopyMB = int64(fv)
		} else {
			errs = append(errs, "max_auto_copy_mb 需要非负数字")
		}
	}
	if v, ok := in["auto_paste"]; ok {
		if bv, ok := v.(bool); ok {
			cfg.AutoPaste = bv
		} else {
			errs = append(errs, "auto_paste 需要布尔值")
		}
	}
	if v, ok := in["text_sync"]; ok {
		if bv, ok := v.(bool); ok {
			cfg.TextSync = bv
		} else {
			errs = append(errs, "text_sync 需要布尔值")
		}
	}
	// 资源管理器右键菜单：开关即注册/注销（HKCU 项），失败则整体不落盘
	oldShellMenu := cfg.ShellMenu
	shellMenuChanged := false
	if v, ok := in["shell_menu"]; ok {
		if bv, ok := v.(bool); ok {
			if cfg.ShellMenuOn() != bv {
				shellMenuChanged = true
			}
			cfg.ShellMenu = &bv
		} else {
			errs = append(errs, "shell_menu 需要布尔值")
		}
	}
	// KVM 布局：允许空串（清除某一侧）；邻居名本机即时热更新，并推送到对端。
	// 记录改动前的值：取消（清空）或换人时向旧邻居推送移除通知。
	oldKVMLeft, oldKVMRight := cfg.KVMLeft, cfg.KVMRight
	kvmChanged := false
	applyKVM := func(key string, dst *string) {
		if v, ok := in[key]; ok {
			sv, ok := v.(string)
			if !ok {
				errs = append(errs, key+" 需要字符串")
				return
			}
			*dst = strings.TrimSpace(sv)
			kvmChanged = true
		}
	}
	applyKVM("kvm_left", &cfg.KVMLeft)
	applyKVM("kvm_right", &cfg.KVMRight)
	kvmEnableChanged := false
	if v, ok := in["kvm_enabled"]; ok {
		if bv, ok := v.(bool); ok {
			if cfg.KVMOn() != bv {
				kvmEnableChanged = true
			}
			cfg.KVMEnabled = &bv
		} else {
			errs = append(errs, "kvm_enabled 需要布尔值")
		}
	}
	// 触控板手势识别（实验性）：开关与速度倍率均即时生效，无需重启
	touchpadChanged := false
	if v, ok := in["kvm_touchpad_gestures"]; ok {
		if bv, ok := v.(bool); ok {
			if cfg.KVMTouchpadOn() != bv {
				touchpadChanged = true
			}
			cfg.KVMTouchpad = &bv
		} else {
			errs = append(errs, "kvm_touchpad_gestures 需要布尔值")
		}
	}
	if v, ok := in["kvm_touchpad_speed"]; ok {
		if fv, ok := v.(float64); ok && fv >= 10 && fv <= 1000 {
			if cfg.KVMTouchpadSpeedPct() != int(fv) {
				touchpadChanged = true
			}
			cfg.KVMTouchSpd = int(fv)
		} else {
			errs = append(errs, "kvm_touchpad_speed 需要 10~1000 的整数（100=基准）")
		}
	}
	// KVM 手感调参：主控移动合拍间隔、被控重排节拍（毫秒）。
	// 与邻居一致走热更新通道（保存即触发 core.SyncKVM → kvm.UpdateTunables），
	// 无需重启。move>=0（0=默认8），reflow 可为 -1（关闭重排）。
	if v, ok := in["kvm_move_interval_ms"]; ok {
		if fv, ok := v.(float64); ok && fv >= 0 {
			cfg.KVMMoveMs = int(fv)
			kvmChanged = true
		} else {
			errs = append(errs, "kvm_move_interval_ms 需要非负整数")
		}
	}
	if v, ok := in["kvm_reflow_step_ms"]; ok {
		if fv, ok := v.(float64); ok && fv >= -1 {
			cfg.KVMReflowMs = int(fv)
			kvmChanged = true
		} else {
			errs = append(errs, "kvm_reflow_step_ms 需要 -1 或非负整数")
		}
	}
	if v, ok := in["kvm_speed_percent"]; ok {
		if fv, ok := v.(float64); ok && fv >= 0 && fv <= 500 {
			cfg.KVMSpeedPct = int(fv)
			kvmChanged = true
		} else {
			errs = append(errs, "kvm_speed_percent 需要 0~500 的整数（100=基准 1.0x）")
		}
	}
	// 边缘死区（逻辑像素）：屏幕上下两端的系统 UI 热区（关闭按钮/显示桌面/
	// 任务栏）不触发跨屏或切回。0 = 关闭死区。热更新到 kvm 层，无需重启。
	if v, ok := in["kvm_edge_dead_zone"]; ok {
		if fv, ok := v.(float64); ok && fv >= 0 && fv <= 400 {
			dz := int(fv)
			cfg.KVMEdgeDZ = &dz
			kvmChanged = true
		} else {
			errs = append(errs, "kvm_edge_dead_zone 需要 0~400 的整数（0=关闭死区）")
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "；"))
	}

	// 注册表操作可能失败（权限/杀软拦截）：先做，失败则回滚开关且不落盘，
	// 避免"配置说开着、菜单其实没注册"的静默不一致。注册表写入还可能被
	// 安全软件长时间挂起，这里加超时，免得整个保存请求跟着卡死。
	if shellMenuChanged {
		on := cfg.ShellMenuOn()
		if err := runWithTimeout(func() error { return core.SetShellMenu(on) }, shellMenuTimeout); err != nil {
			cfg.ShellMenu = oldShellMenu
			return fmt.Errorf("更新资源管理器右键菜单失败: %w", err)
		}
	}
	if err := cfg.Save(cfgPath); err != nil {
		return fmt.Errorf("保存配置失败: %w", err)
	}
	log.Printf("配置已更新并保存")

	if nodeNameChanged {
		// 广播名每次公告时取当前值：下一次广播（约 announce_interval 秒）即生效
		log.Printf("节点名已更新为 %q，将在下一次广播生效", cfg.NodeName)
	}
	if kvmEnableChanged {
		// 跨屏开关即时生效：热启动/停用 KVM 服务与监听，无需重启
		core.SetKVMEnabled(cfg.KVMOn())
	}
	if touchpadChanged {
		// 触控板手势识别与速度倍率即时生效（实验性），无需重启
		core.SetTouchpadGestures(cfg.KVMTouchpadOn())
		core.SetTouchpadSpeed(cfg.KVMTouchpadSpeedPct())
	}
	if kvmChanged {
		// 邻居改动即时生效（本机热更新 + 推送到对端，自动互为镜像；
		// 取消/换人时同步通知旧邻居解除）
		go core.SyncKVMFrom(oldKVMLeft, oldKVMRight)
	}
	return nil
}

// shellMenuTimeout 是注册表菜单操作的上限：安全软件拦截时写入可能长时间挂起，
// 设置页的"保存"不能跟着一起卡死。
const shellMenuTimeout = 5 * time.Second

// runWithTimeout 在 goroutine 里执行 fn 并限制等待时间。
// 超时后 fn 仍可能在后台跑完（无法强杀系统调用），这里只保证调用方不被拖住。
func runWithTimeout(fn func() error, d time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		return errors.New("注册表操作超时（可能被安全软件拦截，请把 copywhere 加入信任列表）")
	}
}
