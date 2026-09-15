# 触控板手势识别（PT_GESTURE2 绕行）方案与 PoC 记录

状态：**PoC 已落地，待真机验证**。对应 README「鼠标键盘跨屏（KVM）」已知问题：
精确式触控板（PTP）双指滚动属于 `PT_GESTURE2` 指针手势，由系统直接投递给光标下
窗口，不经过 WH_MOUSE_LL，也不以滚轮形式出现在鼠标 Raw Input 流中，用户态全局
钩子原理上无法捕获。

## 1. 方案定位与分层可行性

| 层 | 可行性 | 说明 |
| --- | --- | --- |
| L1 监听：注册 PTP 数字化器 Raw Input，拿原始触点帧 | ✅ 可行 | Raw Input 是被动旁路，系统消费 HID 流的同时旁路副本给注册者，不干扰系统 |
| L2 识别：双指滚动状态机 | ✅ 可行但难调好 | 算法不复杂，"调到跟系统手感一致"很难 |
| L3 抑制：让本机不再滚动 | ❌ 用户态不可行 | Raw Input 不能消费输入；唯一用户态开关（Report Mode 切 Touchpad 模式）会连带杀掉系统触控板栈 |

结论：**影子监听**是唯一务实的用户态路径——系统照常处理手势，本机窗口仍会滚动
（双滚动）；KVM 接管期间识别器把滚动转发到副机。相比现状（副机完全收不到）是
严格改善，但替代不了"文档告知 + 外接鼠标/厂商选项"的现有处置。

## 2. PoC 实现（internal/input/touchpad_windows.go）

### 2.1 L1：设备注册与报告解析

- `hookThread` 追加注册第二个 Raw Input 设备：**Page 0x0D（Digitizers）/
  Usage 0x05（Touch Pad）**，`RIDEV_INPUTSINK`，与鼠标共用同一个消息窗口。
  触摸屏等同页设备由 `HidP_GetCaps` 的顶级集合 Usage 过滤排除。
- `handleRawInput` 改为按 `RAWINPUTHEADER.dwType` 分发：`RIM_TYPEHID(2)` 进入
  触控板路径；RAWHID 布局为 `dwSizeHid + dwCount + N 份原始报告`。
- 报告解析走 **preparsed data**（`GetRawInputDeviceInfoW(RIDI_PREPARSEDDATA)`，
  每设备缓存一次），用 hid.dll 的 `HidP_GetCaps / HidP_GetValueCaps /
  HidP_GetUsages / HidP_GetUsageValue` 按 usage 取值，不依赖任何厂商偏移：
  - 触点槽位 = **含 Generic Desktop X(0x30) 的 LinkCollection**（PTP 每触点
    一个链接集合）；每槽位读 X/Y、Contact Identifier(0x51)、Tip Switch(0x42)。
  - **坑**：触点 X/Y 复用 Generic Desktop 页 0x30/0x31，Digitizer 页自己的
    0x30 是 Tip Pressure——凭记忆硬编码必踩。
- 无 preparsed/非触控板/解析不出槽位 → 设备置 nil 并缓存，静默降级
  （宁漏勿误）。

### 2.2 L2：双指滚动状态机

- 恰好 2 个 Tip Switch 按下的触点 → 追踪两触点**质心**位移（按 Contact ID
  跨帧配对）→ 累积器跨过 `touchUnitsPerWheel`（默认 100 设备单位 = 一格
  ±120）即发 `OnWheel`。方向沿用系统默认：双指下移 = 滚轮向下，双指右移 =
  横向轮向右；违和感只需翻转 `drain()` 中的符号。
- 触点数偏离 2（单指/三指以上/抬手）一律退避：不与系统的捏合、三指切换、
  边缘轻扫等手势竞争。
- 输出仅在 `suppress == true`（KVM 主控接管中）时经既有 `Callbacks.OnWheel`
  链路转发——KVM 转发层零改动。

### 2.3 双通道去重（关键兜底）

在"回退 legacy 滚轮"的窗口（边缘落在不可滚动区域），系统会合成
`WM_MOUSEWHEEL`（钩子可见）**且**识别器也在发滚轮 → 双重转发。对策：
`touchEmitWheel` 打时间戳，`mouseHookProc` 丢弃 `touchDedupWindow`（250ms）
内的钩子滚轮。未接管时不去重（本机正常滚动不受任何影响）。

## 3. 验证方法（真机）

1. 构建：`go build -ldflags "-H windowsgui" -o copywhere.exe ./cmd/copywhere`
2. 开关：面板「设置 → 跨屏控制 → 触控板双指滚动（实验性）」或配置文件
   `kvm_touchpad_gestures`（默认关闭）；面板改动热生效，无需重启。PoC 日志
   由 `input.SetTouchpadDebug(true)` 控制（默认关），形如：
   `copywhere/input: 触控板双指滚动 delta=-120 横向=false 接管中=true`
3. 用例矩阵：
   - 接管中，光标压在本机可滚动窗口上双指滚动 → 日志应出现且副机滚动；
   - 接管中，光标压在不可滚动区域双指滚动 → 只应出现一次滚动（去重生效）；
   - 未接管时双指滚动 → 无日志输出、无转发、本机行为完全正常；
   - 双指点击（右键）、捏合、三指轻扫 → 不应产生滚动输出；
   - 掌压边缘打字 → 不应产生滚动输出（Confidence 触点被忽略、tip 判定兜底）。
4. 待调参数：`touchUnitsPerWheel`（速度感基准）、`drain()` 方向符号、
   `touchDedupWindow`。

## 3.1 真机反馈修正（2026-09-15）

- **垂直方向反了** → `drain()` 垂直符号翻转：双指下移（dy>0）= +120。
  横向维持"双指右移 = +120"，如有违和同样一行翻转。
- **速度偏慢、需要可调** → 新增输出倍率：`kvm_touchpad_speed`
  （10~1000，面板滑块 50%~400%，默认 100 基准），`drain()` 的跨阈步长
  按倍率缩放，面板保存即时生效（`App.SetTouchpadSpeed` →
  `input.SetTouchpadSpeed`）。
- **不要与鼠标滚轮事件冲突 → 钩子滚轮权威（真机反馈二修）**：最初方案是
  识别器独占（钩子滚轮丢弃），真机反馈两路同时存在时**只要钩子滚轮**——
  系统合成的滚轮滚动量/惯性更准。现语义：钩子滚轮始终转发；双指手势期间
  一旦出现钩子滚轮（`touchNoteHookWheel` 置 `touchHookOwns`，保持到手势
  起步才清除），识别器本手势内让位；识别器只在系统未合成滚轮的指针手势
  区域输出（本方案的目标场景）。起步另有 100ms 缓冲（`touchHookGrace`）：
  缓冲期内不发（累积保留、放行后一次性补发），让系统先表明是否接管，
  消除首格双发竞态。

## 4. 已知限制与不做的部分

- **双滚动**：接管期间本机窗口跟着滚（L3 用户态不可行，见 §1）。
- **手感差异**：系统滚动加速曲线闭源，自研只有匀速换算，无惯性/动量。
- **机型碎片化**：Synaptics/ELAN/Goodix 对 PTP 规范实现有差异；个别厂商把
  触点坐标声明为数组字段（无独立 LinkCollection），解析退化、识别器自动
  静默。需要机型白名单 + 每机型调参。
- **不做**：Report Mode 切换（会杀死系统触控板栈，崩溃/强杀未切回时触控板
  "报废"）、捏合/三指手势、惯性模拟。内核 HID 过滤驱动是唯一干净解，但需要
  签名驱动，违背"单 exe、零依赖"边界。
- 上线形态建议：默认关闭的实验性配置项（如 `kvm_touchpad_gestures`），文档
  与 README 已知限制章节保持一致定位：**改善而非修复**。

## 5. 里程碑

| 阶段 | 内容 | 状态 |
| --- | --- | --- |
| PoC | L1 解析 + L2 识别 + 去重兜底 + 日志 | ✅ 代码落地，质量门禁通过（build/vet/test/gofmt） |
| MVP | 配置项 `kvm_touchpad_gestures`（默认关闭）+ 面板开关热生效 + README/CHANGELOG | ✅ 代码落地，质量门禁通过 |
| 真机验证 | 2~3 台不同厂商触控板跑 §3 矩阵 | ⬜ |
| 打磨 | 机型参数（`touchUnitsPerWheel`、方向符号）、必要的 UI 提示 | ⬜ |

不做（风险/收益不成比例）：Report Mode 切换、捏合/三指手势、惯性模拟。
