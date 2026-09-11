//go:build !windows

// Package input 的非 Windows 占位实现：KVM 功能仅支持 Windows。
package input

// Rect 是虚拟桌面坐标系中的一个矩形。
type Rect struct {
	X, Y, W, H int
	Primary    bool
}

// Callbacks 输入事件回调集合（非 Windows 平台不会被调用）。
type Callbacks struct {
	OnMouseMove     func(dx, dy int)
	OnMouseButton   func(down bool, button int, x, y int)
	OnWheel         func(delta int32, horizontal bool)
	OnKey           func(vk, scan uint32, down, ext bool)
	OnLocalActivity func()
}

// SetSuppress 无操作。
func SetSuppress(bool) {}

// Start 非 Windows 平台不可用。
func Start(cb Callbacks) error {
	return errNotSupported
}

// DefaultInjector 非 Windows 占位注入器（全部无操作）。
type DefaultInjector struct{}

func (DefaultInjector) MoveAbs(int, int)                   {}
func (DefaultInjector) MoveRel(int, int)                   {}
func (DefaultInjector) Button(bool, int)                   {}
func (DefaultInjector) Wheel(int32, bool)                  {}
func (DefaultInjector) Key(uint32, uint32, bool, bool)     {}
func (DefaultInjector) ScreenBounds() (int, int, int, int) { return 0, 0, 0, 0 }
func (DefaultInjector) CursorPos() (int, int)              { return 0, 0 }
func (DefaultInjector) Monitors() []Rect                   { return nil }
func (DefaultInjector) MouseSpeed() int                    { return 0 }

// MouseSpeed 非 Windows 占位（返回 0 表示不补偿）。
func MouseSpeed() int { return 0 }

// CursorPos 非 Windows 占位。
func CursorPos() (int, int) { return 0, 0 }

// SetCursorPos 非 Windows 占位。
func SetCursorPos(int, int) {}

var errNotSupported = errStr("copywhere 的 KVM 功能仅支持 Windows")

type errStr string

func (e errStr) Error() string { return string(e) }
