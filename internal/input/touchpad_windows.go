//go:build windows

// 触控板手势识别（PoC）：旁路监听 Windows 精确式触控板（PTP）数字化器集合的
// Raw Input，自研识别"双指滚动"，在 KVM 主控接管期间转换为滚轮事件转发。
//
// 背景：PTP 双指滚动由系统作为指针手势（PT_GESTURE2）直接投递给光标下窗口，
// 不经过 WH_MOUSE_LL，也不以滚轮形式出现在鼠标 Raw Input 流中，用户态全局
// 钩子原理上无法捕获（见 README「鼠标键盘跨屏（KVM）」已知问题）。本文件
// 绕过指针层，直接消费触控板 HID 输入报告（Raw Input 是被动旁路，不影响
// 系统自身的触控板处理）。
//
// 已知限制（PoC 定位）：
//   - 影子监听：系统照常处理同一份手势数据，本机窗口仍会滚动（双滚动），
//     只在 KVM 接管期间转发，未接管时不产生任何副作用；
//   - 滚轮来源仲裁：钩子滚轮（系统合成的 legacy 滚轮）是权威源——双指
//     手势期间一旦出现钩子滚轮，识别器本手势内让位；识别器只在系统
//     未合成滚轮的指针手势区域输出（本方案的目标场景），并有起步缓冲
//     避免首格双发（touchNoteHookWheel / touchHookGrace）；
//   - 仅识别双指滚动，不处理捏合 / 三指 / 轻扫等系统手势（主动退避）；
//   - 设备单位 → 滚轮换算系数为经验值（touchUnitsPerWheel），按机型手调；
//   - 个别厂商把触点坐标声明为数组字段（无独立 LinkCollection），此时解析
//     退化、识别器看不到第二个手指，自动静默不工作（宁漏勿误）。
//
// 权威依据：
//   - HIDP_CAPS / HIDP_VALUE_CAPS：learn.microsoft.com hidpi.h 文档；
//   - Digitizer usage：USB HID Usage Tables（Tip Switch 0x42、Confidence
//     0x47、Width 0x48、Height 0x49、Contact Identifier 0x51、Contact Count
//     0x54、Scan Time 0x56）；触点 X/Y 复用 Generic Desktop 页 0x30/0x31
//     （Digitizer 页自己的 0x30 是 Tip Pressure，不是坐标！）。
package input

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	rimTypeHid        = 2          // RAWINPUTHEADER.dwType: RIM_TYPEHID
	ridiPreparsedData = 0x20000005 // GetRawInputDeviceInfo: RIDI_PREPARSEDDATA
	hidpInput         = 0          // HIDP_REPORT_TYPE: HidP_Input
	hidpStatusSuccess = 0x00110000 // HIDP_STATUS_SUCCESS

	usagePageDigitizer = 0x0D
	usagePageGenDesk   = 0x01

	dgTouchpad     = 0x05 // Touch Pad 顶级/链接集合
	dgTipSwitch    = 0x42 // 触点是否在面板上（按下状态）
	dgContactID    = 0x51 // 跨帧触点标识
	dgContactCount = 0x54 // 本报告触点数

	gdX = 0x30 // Generic Desktop X（触点绝对坐标）
	gdY = 0x31 // Generic Desktop Y

	// touchUnitsPerWheel：设备单位位移 ≈ 一格滚轮（±120）的经验换算系数。
	// 常见 PTP 触控板 1cm 指尖位移 ≈ 250~350 单位，一格滚轮 ≈ 1/3 屏行高，
	// 100 为折中初值；机型适配时按手感手调。
	touchUnitsPerWheel = 100.0

	// touchTwoFingerWindow：识别器最近见过"恰有两指按下"的帧即认为双指
	// 手势进行中——鼠标钩子据此判断滚轮消息是否来自触控板手势，置
	// touchHookOwns 让识别器让位（见 touchpadTwoFingerRecent）。
	touchTwoFingerWindow = 150 * time.Millisecond

	// touchHookGrace：双指手势起步缓冲——给系统一点时间表明它是否正在
	// 把该手势合成为 legacy 滚轮；缓冲期内识别器不发（累积保留，缓冲期
	// 满且钩子未接管时一次性补发），避免起步首格双发。
	touchHookGrace = 100 * time.Millisecond

	touchMaxDevices = 8 // 防御性上限：缓存的触控板设备数
)

var (
	hid = windows.NewLazySystemDLL("hid.dll")

	procHidPGetCaps       = hid.NewProc("HidP_GetCaps")
	procHidPGetValueCaps  = hid.NewProc("HidP_GetValueCaps")
	procHidPGetUsages     = hid.NewProc("HidP_GetUsages")
	procHidPGetUsageValue = hid.NewProc("HidP_GetUsageValue")

	procGetRawInputDeviceInfoW = user32.NewProc("GetRawInputDeviceInfoW")
)

// hidpCaps 与 C 的 HIDP_CAPS 布局一致（全 uint16：Usage/UsagePage 2 + 三类
// 报告长度 3 + Reserved[17] + Number* 10 = 32 个字段，64 字节）。
type hidpCaps struct {
	usage, usagePage          uint16
	inputReportByteLength     uint16
	outputReportByteLength    uint16
	featureReportByteLength   uint16
	reserved                  [17]uint16
	numberLinkCollectionNodes uint16
	numberInputButtonCaps     uint16
	numberInputValueCaps      uint16
	numberInputDataIndices    uint16
	numberOutputButtonCaps    uint16
	numberOutputValueCaps     uint16
	numberOutputDataIndices   uint16
	numberFeatureButtonCaps   uint16
	numberFeatureValueCaps    uint16
	numberFeatureDataIndices  uint16
}

// hidpValueCaps 与 C 的 HIDP_VALUE_CAPS 布局一致（72 字节，按官方文档字段
// 顺序逐字段排列）。Range/NotRange 联合体只取前两个 usage 字段足够。
type hidpValueCaps struct {
	usagePage                                        uint16
	reportID                                         uint8
	isAlias                                          uint8
	bitField                                         uint16
	linkCollection                                   uint16
	linkUsage                                        uint16
	linkUsagePage                                    uint16
	isRange                                          uint8
	isStringRange                                    uint8
	isDesignatorRange                                uint8
	isAbsolute                                       uint8
	hasNull                                          uint8
	_                                                uint8
	bitSize                                          uint16
	reportCount                                      uint16
	_                                                [5]uint16
	unitsExp, units                                  uint32
	logicalMin, logicalMax, physicalMin, physicalMax int32
	// NotRange: Usage / Range: UsageMin（本文件只关心 usage 主体）
	usageMin, usageMax uint16
	_                  [6]uint16
}

// 运行时布局护栏：FFI 结构体尺寸必须与 C 一致，否则触控板识别整体禁用
// （比编译期断言啰嗦，但不会因 const 表达式限制误伤）。
var touchLayoutOK = func() bool {
	if unsafe.Sizeof(hidpCaps{}) != 64 || unsafe.Sizeof(hidpValueCaps{}) != 72 {
		fmt.Println("copywhere/input: HID 结构体布局异常，触控板手势识别禁用")
		return false
	}
	return true
}()

// touchContact 是从一份触控板报告解析出的单个触点。
type touchContact struct {
	id  uint32
	x   int32
	y   int32
	tip bool // Tip Switch：触点在面板上
}

// touchpadDev 是一台已解析能力描述的触控板。
type touchpadDev struct {
	preparsed    []byte   // 持有 preparsed data 存储（句柄即缓冲区指针）
	handle       uintptr  // PHIDP_PREPARSED_DATA
	slots        []uint16 // 触点槽位 = 含 X 的 LinkCollection 下标（PTP 每触点一个集合）
	contactCount uint16   // Contact Count 所在 LinkCollection（诊断用）
	rec          touchRecognizer
}

var (
	touchDevsMu sync.Mutex
	touchDevs   = map[uintptr]*touchpadDev{} // hDevice → 设备（失败也缓存，避免每帧重试）

	touchTwoFingerAt  atomic.Int64 // 最近一次"恰有两指按下"帧的 UnixNano（钩子让位判定用）
	touchGestureStart atomic.Int64 // 当前双指手势起步时刻的 UnixNano（起步缓冲用）
	touchHookOwns     atomic.Bool  // 本手势系统已合成 legacy 滚轮 → 识别器让位（钩子滚轮权威）
	touchDebugFlag    atomic.Bool  // PoC 日志开关
	touchEnabledFlag  atomic.Bool  // 手势识别总开关（默认关闭，实验性）
	touchSpeedPct     atomic.Int32 // 滚动输出倍率百分比（100=基准；0 视为 100）
)

// SetTouchpadGestures 开关触控板手势识别（实验性，默认关闭）。
func SetTouchpadGestures(b bool) { touchEnabledFlag.Store(b) }

// SetTouchpadSpeed 设置双指滚动的输出倍率百分比（100=基准，越大越快）。
func SetTouchpadSpeed(pct int) {
	if pct <= 0 {
		pct = 100
	}
	if pct < 10 {
		pct = 10
	} else if pct > 1000 {
		pct = 1000
	}
	touchSpeedPct.Store(int32(pct))
}

// SetTouchpadDebug 开关触控板识别的 PoC 日志。
func SetTouchpadDebug(b bool) { touchDebugFlag.Store(b) }

// touchpadTwoFingerRecent 报告最近 touchTwoFingerWindow 内是否见过
// "恰有两指按下"的帧（双指手势进行中）。鼠标钩子据此判断当前滚轮
// 消息是否可能来自触控板手势，进而置 touchHookOwns 让识别器让位。
func touchpadTwoFingerRecent() bool {
	t := touchTwoFingerAt.Load()
	return t > 0 && time.Since(time.Unix(0, t)) < touchTwoFingerWindow
}

// touchNoteHookWheel 由鼠标钩子调用：双指手势进行中出现了 legacy 滚轮，
// 说明系统已把该手势合成为滚轮消息（"回退 legacy 滚轮"区域）——钩子
// 滚轮是权威源（系统换算的滚动量/惯性更准），识别器本手势内让位。
// 置位保持到下一次双指手势起步时清除（touchRecognizer.update），
// 手势中途停顿超过窗口也不会翻转回识别器。
func touchNoteHookWheel() { touchHookOwns.Store(true) }

// handleTouchpadInput 处理一帧 Raw Input HID 数据（RAWHID 布局：
// dwSizeHid、dwCount、若干份原始报告）。
func handleTouchpadInput(h *rawinputHeader, payload []byte) {
	defer func() { _ = recover() }() // 钩子线程绝不能 panic
	if !touchEnabledFlag.Load() {
		return // 总开关关闭：不解析不识别（默认态，仅剩一次原子读的开销）
	}
	if len(payload) < 8 {
		return
	}
	dev := touchpadGet(h.hDevice)
	if dev == nil {
		return
	}
	perReport := int(*(*uint32)(unsafe.Pointer(&payload[0])))
	count := int(*(*uint32)(unsafe.Pointer(&payload[4])))
	if perReport <= 0 || count <= 0 || perReport > 256 {
		return
	}
	reports := payload[8:]
	for i := 0; i < count && (i+1)*perReport <= len(reports); i++ {
		dev.handleReport(reports[i*perReport : (i+1)*perReport])
	}
}

// touchpadGet 返回（并按需懒加载）hDevice 对应的触控板设备。
func touchpadGet(hDevice uintptr) *touchpadDev {
	touchDevsMu.Lock()
	defer touchDevsMu.Unlock()
	if d, ok := touchDevs[hDevice]; ok {
		return d
	}
	var d *touchpadDev
	if touchLayoutOK && len(touchDevs) < touchMaxDevices {
		d = loadTouchpadDev(hDevice)
	}
	touchDevs[hDevice] = d
	return d
}

// loadTouchpadDev 通过 preparsed data 判定设备是否为精确式触控板，
// 并枚举触点槽位（含 Generic Desktop X 的 LinkCollection）。
func loadTouchpadDev(hDevice uintptr) *touchpadDev {
	var size uint32
	procGetRawInputDeviceInfoW.Call(hDevice, ridiPreparsedData, 0, uintptr(unsafe.Pointer(&size)))
	if size == 0 {
		return nil
	}
	buf := make([]byte, size)
	r, _, _ := procGetRawInputDeviceInfoW.Call(hDevice, ridiPreparsedData,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)))
	if r == ^uintptr(0) || r == 0 {
		return nil
	}
	pp := uintptr(unsafe.Pointer(&buf[0]))

	var caps hidpCaps
	if st, _, _ := procHidPGetCaps.Call(pp, uintptr(unsafe.Pointer(&caps))); st != hidpStatusSuccess {
		return nil
	}
	if caps.usagePage != usagePageDigitizer || caps.usage != dgTouchpad {
		return nil // 不是触控板集合（触摸屏等同页设备在此被排除）
	}
	n := int(caps.numberInputValueCaps)
	if n == 0 || n > 512 {
		return nil
	}
	vc := make([]hidpValueCaps, n)
	vlen := uint16(n)
	if st, _, _ := procHidPGetValueCaps.Call(hidpInput, uintptr(unsafe.Pointer(&vc[0])),
		uintptr(unsafe.Pointer(&vlen)), pp); st != hidpStatusSuccess || int(vlen) == 0 {
		return nil
	}

	d := &touchpadDev{preparsed: buf, handle: pp}
	seen := map[uint16]bool{}
	for i := range vc {
		c := &vc[i]
		if c.isRange != 0 {
			continue
		}
		switch {
		case c.usagePage == usagePageGenDesk && c.usageMin == gdX:
			if !seen[c.linkCollection] { // 触点槽位（PTP 规范：X/Y 同集合）
				seen[c.linkCollection] = true
				d.slots = append(d.slots, c.linkCollection)
			}
		case c.usagePage == usagePageDigitizer && c.usageMin == dgContactCount:
			d.contactCount = c.linkCollection
		}
	}
	if len(d.slots) == 0 {
		if touchDebugFlag.Load() {
			fmt.Println("copywhere/input: 触控板未解析出触点槽位（坐标可能为数组字段），忽略")
		}
		return nil
	}
	if touchDebugFlag.Load() {
		fmt.Printf("copywhere/input: 触控板已启用（触点槽位 %d 个，报告 %d 字节）\n",
			len(d.slots), caps.inputReportByteLength)
	}
	return d
}

// handleReport 解析一份原始报告（含报告 ID 字节）并喂给识别器。
func (d *touchpadDev) handleReport(report []byte) {
	if len(report) == 0 {
		return
	}
	cs := make([]touchContact, 0, len(d.slots))
	for _, link := range d.slots {
		var c touchContact
		var btns [8]uint16
		n := uint32(len(btns))
		st, _, _ := procHidPGetUsages.Call(hidpInput, uintptr(usagePageDigitizer), uintptr(link),
			uintptr(unsafe.Pointer(&btns[0])), uintptr(unsafe.Pointer(&n)),
			d.handle, uintptr(unsafe.Pointer(&report[0])), uintptr(len(report)))
		if st == hidpStatusSuccess {
			for i := uint32(0); i < n; i++ {
				if btns[i] == dgTipSwitch {
					c.tip = true
				}
			}
		}
		var x, y, id uint32
		if !hidGetUsageValue(d.handle, usagePageGenDesk, link, gdX, &x, report) ||
			!hidGetUsageValue(d.handle, usagePageGenDesk, link, gdY, &y, report) {
			continue
		}
		if !hidGetUsageValue(d.handle, usagePageDigitizer, link, dgContactID, &id, report) {
			id = uint32(link) // 无 ContactID 时以槽位序号代替
		}
		c.id, c.x, c.y = id, int32(x), int32(y)
		cs = append(cs, c)
	}
	d.rec.update(cs)
}

// hidGetUsageValue 按 usage（限定 LinkCollection）从报告中取标量值。
func hidGetUsageValue(pp uintptr, page, link, usage uint16, out *uint32, report []byte) bool {
	st, _, _ := procHidPGetUsageValue.Call(hidpInput, uintptr(page), uintptr(link), uintptr(usage),
		uintptr(unsafe.Pointer(out)), pp, uintptr(unsafe.Pointer(&report[0])), uintptr(len(report)))
	return st == hidpStatusSuccess
}

// touchRecognizer 是单设备的双指滚动状态机：
// 恰好 2 个 Tip Switch 按下的触点 → 追踪质心位移 → 跨过阈值发滚轮；
// 触点数偏离 2（单指 / 三指以上 / 抬手）一律退避，不与系统手势竞争。
type touchRecognizer struct {
	prev         map[uint32][2]int32 // 上一帧 tip=1 的触点（id → x,y）
	active       bool
	lastX, lastY float64 // 质心
	accX, accY   float64 // 未满一格的累积位移
}

func (r *touchRecognizer) update(cs []touchContact) {
	cur := make(map[uint32][2]int32, len(cs))
	for _, c := range cs {
		if c.tip {
			cur[c.id] = [2]int32{c.x, c.y}
		}
	}
	if len(cur) == 2 {
		touchTwoFingerAt.Store(time.Now().UnixNano()) // 双指手势进行中（钩子让位判定）
		var sx, sy float64
		for _, p := range cur {
			sx += float64(p[0])
			sy += float64(p[1])
		}
		cx, cy := sx/2, sy/2
		if !r.active || len(r.prev) == 0 {
			// 双指刚按下：新手势起步——清钩子让位标记、记起步时刻
			//（起步缓冲期内识别器不发，等系统表明是否合成 legacy 滚轮）
			r.active = true
			r.accX, r.accY = 0, 0
			r.lastX, r.lastY = cx, cy
			r.prev = cur
			touchHookOwns.Store(false)
			touchGestureStart.Store(time.Now().UnixNano())
			return
		}
		r.accX += cx - r.lastX
		r.accY += cy - r.lastY
		r.lastX, r.lastY = cx, cy
		r.drain()
	} else {
		r.active = false
	}
	r.prev = cur
}

// drain 把累积位移折算成滚轮。方向沿用真机校准结果：双指下移（dy>0）=
// 滚轮向上（+120），双指右移（dx>0）= 滚轮向左（-120，横向轮，已按需求左右对调）。
// 输出倍率由 touchSpeedPct 控制（kvm_touchpad_speed，越大越快）。
// touchEmitWheel 拒收（钩子滚轮权威 / 起步缓冲）时保留累积、停止本轮
// 折算，待门控放行后的下一帧一次性补发。
// 方向若仍有违和感，翻转对应轴的两处符号即可（一行为一处）。
func (r *touchRecognizer) drain() {
	step := touchUnitsPerWheel * 100 / float64(touchSpeed())
	for r.accY >= step {
		if !touchEmitWheel(120, false) {
			break
		}
		r.accY -= step
	}
	for r.accY <= -step {
		if !touchEmitWheel(-120, false) {
			break
		}
		r.accY += step
	}
	for r.accX >= step {
		if !touchEmitWheel(-120, true) {
			break
		}
		r.accX -= step
	}
	for r.accX <= -step {
		if !touchEmitWheel(120, true) {
			break
		}
		r.accX += step
	}
}

// touchSpeed 返回当前输出倍率百分比（0/未设置视为 100）。
func touchSpeed() int {
	pct := int(touchSpeedPct.Load())
	if pct <= 0 {
		return 100
	}
	return pct
}

// touchEmitWheel 尝试输出一次识别出的滚轮事件，返回是否真正转发。
// 门控（按优先级）：
//  1. 起步缓冲（touchHookGrace 内）：等系统表明是否合成 legacy 滚轮；
//  2. 钩子滚轮权威（touchHookOwns）：本手势系统已在合成滚轮，
//     识别器让位——两路同时存在时只要钩子滚轮（真机反馈定案）。
//
// 放行且 KVM 主控接管中（suppress）时经既有 OnWheel 链路转发。
func touchEmitWheel(delta int32, horizontal bool) bool {
	now := time.Now()
	if t := touchGestureStart.Load(); t > 0 && now.Sub(time.Unix(0, t)) < touchHookGrace {
		return false
	}
	if touchHookOwns.Load() {
		return false
	}
	if touchDebugFlag.Load() {
		fmt.Printf("copywhere/input: 触控板双指滚动 delta=%d 横向=%v 接管中=%v\n",
			delta, horizontal, suppress.Load())
	}
	if !suppress.Load() {
		return true
	}
	if cbs.OnWheel != nil {
		cbs.OnWheel(delta, horizontal)
	}
	return true
}
