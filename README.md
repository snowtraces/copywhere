# copywhere

<p align="left">
  <img src="internal/wailsui/frontend/favicon.png" width="72" alt="copywhere 图标">
</p>

**局域网剪贴板 / 文件自动同步 + 鼠标键盘跨屏工具**（Windows，Go 实现，单 exe；界面依赖系统 WebView2 运行时）。

- **剪贴板同步**：在一台机器上 **Ctrl+C** 复制小于阈值的文件（默认 10MB）、文本或截图，
  局域网内所有在线 `copywhere` 节点会自动收到：文件落盘到接收目录并写回其剪贴板，
  对端直接 **Ctrl+V** 即可粘贴。
- **鼠键跨屏（软 KVM）**：把光标推向屏幕边缘即可用本机鼠标键盘控制相邻机器，
  键盘/滚轮同步转发，多机布局在桌面窗口的「屏幕布局」里配置、自动互相同步。

无需配置 IP、无需账号、无需中转服务器——数据只在你的机器之间点对点直传。

![Platform](https://img.shields.io/badge/platform-Windows-blue) ![Go](https://img.shields.io/badge/Go-1.26.4%2B-00ADD8) ![License](https://img.shields.io/badge/license-MIT-green)

## 特性一览

- **节点自动发现**：UDP 广播（各网段定向广播 + 回环），无需配置 IP，节点上下线自动感知
- **配对认证**：窗口点击「配对」→ 对端「接受配对」即完成；双方交换逐对独立的配对
  令牌（`~/.copywhere/peers.json`，可单独解除），未配对的节点无法传输
- **Wails v3 桌面窗口（唯一图形界面）**：双击 exe 即开独立 WebView2 窗口——
  对话式传输工作台、设备坞、屏幕布局（KVM）、设置、运行日志、深浅双主题；
  界面由进程内 Go 绑定直接驱动（无本地 HTTP 端口），文件拖入即拿真实路径投送，
  关闭窗口收进托盘继续同步，二次启动自动唤回已有窗口
- **剪贴板全类型同步**：文件（CF_HDROP）、文本（含 HTML/RTF 富格式，跨机粘贴
  保留样式）、截图（Win+Shift+S / PrintScreen 直接写入剪贴板的图片）自动同步；
  接收端自动回贴。事件驱动检测（`AddClipboardFormatListener`，毫秒级）+ 轮询兜底
- **多文件 / 文件夹**：自动打包 zip 传输，对端**自动解包**后回贴原内容（文件夹仍是
  文件夹、多文件仍是多文件）；用户亲手发送的 zip 不误解包
- **手动发送**：`copywhere send` 或窗口拖入文件不受阈值限制（单文件上限 4GB）
- **右键发送**：资源管理器里右键文件/文件夹 →「用 copywhere 发送」即投送到全部在线节点
  （不受阈值限制；多选自动合并成一次发送，文件夹自动打包）；注册项由窗口启动时自动
  指向当前 exe，可在窗口「设置 → 通用」随时关闭。helper 通过**本机命名管道 IPC**
  把路径交给运行中的实例，不依赖 HTTP 面板
- **可靠传输**：TCP + sha256 校验 + 临时文件原子改名；每次接收落在独立子目录，
  文件保留原始文件名
- **多网卡地址合并**：同一节点从多个网络（局域网 + ZeroTier 等）广播只算一个节点，
  全部地址保留并优选排序，发送时逐地址尝试，任一可达即成功
- **慢链路友好**：超时预算按 256KB/s 估算（ZeroTier 中继等慢链路不误杀），失败原因记录在对端日志
- **容错**：对端短暂离线时内容暂存 60 秒自动重试；配对被拒后提示一次即暂停、不高频重试，
  完成配对后自动恢复；剪贴板序号 + 冷却期双重防回环
- **鼠标键盘跨屏（实验性）**：光标推向显示器"暴露边缘"并继续推动即可控制相邻机器，
  键盘/滚轮同步，布局在窗口「屏幕布局」配置并自动同步到对端；屏幕上下两端留死区
  防误触（关闭按钮/显示桌面/任务栏不跨屏）；`Ctrl+Alt+Shift+X` 紧急退出

## 安装

### 下载预编译 exe

从 [GitHub Releases](../../releases) 下载 `copywhere-vX.Y.Z-windows-amd64.exe`，
双击即可运行——首次启动自动生成默认配置（节点名默认为主机名，可在窗口「设置」修改）。
需要系统 WebView2 运行时（Windows 10 1803+ / Windows 11 一般已预装；缺失时请安装
Microsoft Edge WebView2 Runtime）。

### 从源码构建

要求 Go 1.26.4+（Windows）。

```powershell
git clone <本仓库>
cd copywhere

# 开发构建（控制台子系统；双击启动时程序会自行隐藏控制台窗口）
go build -o copywhere.exe ./cmd/copywhere

# 发布构建（注入版本号，与 CHANGELOG / git tag 保持一致）
$v = "v0.6.0"
go build -trimpath -ldflags "-s -w -X copywhere/internal/app.Version=$v" `
  -o copywhere-$v-windows-amd64.exe ./cmd/copywhere

go test ./...        # 运行全部单元测试
copywhere version    # 查看版本号
```

> 发布构建**不要**加 `-H windowsgui`：GUI 子系统会让 PowerShell 不等待进程，
> `version` / `nodes` / `send` 等子命令会表现为"没有输出、立刻返回"，
> `|`、`>` 与退出码也都不可用。双击不弹黑框由程序内的 `hideOwnConsole()` 负责。

exe 图标与版本信息由 `cmd/copywhere/rsrc_windows_amd64.syso` 提供（`go build` 自动嵌入）；
重新生成方式见 [docs/release.md](docs/release.md)。

- 依赖系统 WebView2 运行时（Windows 10 1803+ / Windows 11 一般已预装；
  缺失时请安装 Microsoft Edge WebView2 Runtime）。
- 不使用 Node / npm：前端是内嵌的静态 HTML/CSS/JS，`go build` 一条命令完成。
- 双击 exe（无参数）即进入桌面窗口；`copywhere wails` 等价。
- 设计说明、验证记录与已知限制见 [docs/wails-ui.md](docs/wails-ui.md)。

## 快速开始（两台机器 A、B）

1. **启动**：两台机器上分别**双击 `copywhere.exe`**——托盘常驻并打开桌面窗口。
   首次运行 Windows 会弹防火墙询问，选择「允许」（至少专用网络）——发现/传输/KVM
   三个端口（默认 47830~47832）需要允许入站。

2. **验证发现**：窗口左侧设备坞应出现对方的设备卡片（含全部地址与活跃时间）。

3. **配对**：点击对方节点卡片上的「配对」，对端窗口弹出「接受配对」确认后即完成。
   未配对的节点无法传输。

4. **使用**：在 A 上复制一个小文件（或把文件直接拖进窗口）→ B 的窗口工作台出现
   消息气泡，在 B 上直接 **Ctrl+V** 即可粘贴；复制文本、按 Win+Shift+S 截图同理。

## 使用指南

### 桌面窗口（推荐）

双击 `copywhere.exe`（或执行 `copywhere wails`）启动完整服务 + 系统托盘 + WebView2
桌面窗口，不再打开浏览器：

- **顶栏**：本机名/地址、同步状态、暂停、运行日志、主题、设置；关闭窗口 = 收进托盘
- **设备坞**：本机与在线节点卡片（全部地址、活跃时间、配对状态），
  配对 / 解除配对 / 设为左邻右邻直接在卡片上完成
- **传输工作台**：左收右发的对话式时间线，文本点击展开、一键复制回剪贴板；
  文件显示类型徽标卡片；接收目录内的图片显示缩略图（点击灯箱、双击打开文件）；
  进行中的收发均有实时进度条；记录按 今天/昨天/日期 分组
- **发送**：Enter 发文本（Shift+Enter 换行）、附件按钮选择文件/文件夹，
  或把文件拖进窗口——拿到的是**真实路径**，无需先上传临时目录
- **右键发送**：资源管理器选中文件/文件夹 → 右键 →「用 copywhere 发送」，
  立即投送到全部在线节点；多选在 400ms 内合并成一次发送（对端收到压缩包并自动解包）。
  失败（未启动、未配对）由 helper 弹窗说明原因
- **屏幕布局**（KVM）：左邻 / 本机 / 右邻三屏画布，拖拽节点或点「左 / 右」即配置，
  即时生效并**自动同步到对端**（互为镜像）
- **设置**：节点名 / 阈值 / 接收目录 / 自动回贴 / 文本同步 / 右键菜单 / 跨屏开关与调参
  （含边缘死区）/ 已配对设备管理；所有修改**立即保存并即时生效**，无需重启
- **运行日志**：`Ctrl+L` 抽屉（错误行着色、未读角标）；`Ctrl+K` 聚焦输入、
  `Ctrl+,` 设置、`Esc` 关闭弹层

**系统托盘**菜单：打开窗口 · 自动同步（勾选）· 打开接收目录 · 退出。
关闭窗口只是收进托盘；再次双击 exe 会**唤回已有窗口**而不是新开进程。

**安全**：界面完全在进程内（Go 绑定 + WebView2 事件），不监听任何本地 HTTP 端口；
传输鉴权仍只认配对令牌。`wails` 与 `run` 通过命名互斥锁互斥。

### 无窗口服务（`run`）

适合服务器 / SSH 等没有桌面的环境：

```powershell
copywhere run [-config 路径]
```

启动发现 / 传输 / 剪贴板 / KVM 服务，日志输出到标准输出，`Ctrl+C` 退出；
没有窗口与托盘，也不提供交互式界面。

### 命令

| 命令 | 说明 |
| --- | --- |
| `copywhere`（无参数，或**直接双击 exe**） | 进入桌面窗口模式；双击启动时自动隐藏控制台窗口，托盘常驻 |
| `copywhere init` | 生成默认配置 |
| `copywhere run [-config 路径]` | 启动无窗口服务（日志到标准输出，适合服务器 / SSH） |
| `copywhere wails [-config 路径]` | 启动服务 + 系统托盘 + 桌面窗口（与 `run` 互斥） |
| `copywhere nodes [-config 路径] [-wait 4s]` | 列出在线节点；**已有实例在运行**时直接查询该实例（发现端口被它占用，子进程不再重复监听） |
| `copywhere send [-config 路径] [-text 内容] [-wait 4s] 文件/目录...` | 立即发送到所有在线节点，不受阈值限制；有实例在运行时交给它发送（含 `-text`），不重复投送到本机 |
| `copywhere shell [-install\|-uninstall\|-status]` | 管理资源管理器右键菜单「用 copywhere 发送」（窗口启动时自动注册） |
| `copywhere shell-send 文件/目录...` | 右键菜单内部调用：经命名管道把路径交给运行中的窗口实例 |
| `copywhere clip-test` | 本机剪贴板读写自检（覆盖并尝试恢复剪贴板） |
| `copywhere doctor` | 环境自检：权限/配置/单实例/IPC/WebView2/端口/右键菜单/托盘/日志 |
| `copywhere version` | 显示版本号 |

`copywhere help` 原文：

```text
copywhere — 局域网剪贴板/文件自动同步工具

用法:
  copywhere init                            生成默认配置
  copywhere run [-config 路径]              启动无窗口服务（日志到标准输出，适合服务器 / SSH）
  copywhere wails [-config 路径]            启动服务 + 系统托盘 + Wails v3 桌面窗口
                                            （WebView2；无参数双击 exe 即进入该模式）
  copywhere nodes [-config 路径] [-wait 4s] 列出在线节点（有实例运行时直接查询该实例）
  copywhere send [-config 路径] [-text 内容] [-wait 4s] 文件/目录...
                                            立即发送到所有在线节点（不受大小阈值限制；
                                            有实例运行时交给该实例发送）
  copywhere shell [-install|-uninstall|-status]
                                            管理资源管理器右键菜单「用 copywhere 发送」
  copywhere shell-send 文件/目录...         右键菜单调用：交给正在运行的实例发送
  copywhere clip-test                       本机剪贴板读写自检
  copywhere doctor                          环境自检（权限/配置/端口/WebView2/菜单/日志）
  copywhere version                         显示版本号

工作方式:
  - 直接双击 copywhere.exe = 桌面窗口模式（隐藏控制台窗口，托盘常驻 +
    WebView2 窗口；关闭窗口收进托盘，二次启动唤回已有窗口）
  - 各节点通过 UDP 广播自动发现彼此，无需配置 IP；同一节点多网卡地址自动去重
  - 配对认证：在窗口「在线节点」点击「配对」，对端确认后即完成，
    双方交换独立配对令牌（peers.json），可随时解除配对；
    未配对的节点无法传输（共享 token 认证已废弃）
  - run 运行期间，复制（Ctrl+C）小于阈值的文件或文本，会自动推送到所有在线节点
  - 对端收到文件后保存到接收目录并自动写回其剪贴板，直接 Ctrl+V 即可
  - 在资源管理器里右键文件/文件夹 →「用 copywhere 发送」= 立即发送，
    不受阈值限制；该项由桌面窗口启动时注册（可在窗口「设置 → 通用」关闭）
  - 桌面窗口运行时，nodes / send / shell-send 都通过本机命名管道交给它处理
    （发现端口与配对状态都在该进程里，子进程不再重复监听）

配置文件: ~/.copywhere/config.json（接收目录: ~/.copywhere/files）
```

## 配置（`~/.copywhere/config.json`）

| 字段 | 默认 | 说明 |
| --- | --- | --- |
| `node_name` | 主机名 | 节点显示名 |
| `discovery_port` | 47830 | UDP 发现端口 |
| `transfer_port` | 47831 | TCP 传输端口 |
| `max_auto_copy_mb` | 10 | 剪贴板自动同步阈值（MB），0 = 不限制 |
| `receive_dir` | `~/.copywhere/files` | 接收文件保存目录（每次接收在其下建独立子目录 `<时间戳>-<发送方>`；截图保存在其下「截图」子目录） |
| `auto_paste` | true | 收到文件后自动写入本机剪贴板 |
| `text_sync` | true | 同步剪贴板文本 |
| `shell_menu` | true | 资源管理器右键菜单「用 copywhere 发送」（窗口启动时注册；窗口设置关闭后即时移除） |
| `announce_interval_sec` | 2 | 广播间隔 |
| `peer_ttl_sec` | 12 | 节点存活判定时长（约为广播间隔的 5~6 倍） |
| `kvm_enabled` | true | 鼠标键盘跨屏开关（窗口内热启停） |
| `kvm_port` | 47832 | KVM 监听端口 |
| `kvm_left` / `kvm_right` | 空 | 左/右边缘邻居的**节点名**（可在窗口「屏幕布局」拖动配置，自动同步到对端） |
| `kvm_entry_monitor` | -1 | 被控入口显示器下标（`-1` = 恒用主显示器） |
| `kvm_move_interval_ms` | 0（=8） | 主控移动合拍间隔（ms），越小越跟手但更耗带宽；**窗口保存后立即生效** |
| `kvm_reflow_step_ms` | 0（=4） | 被控重排注入节拍（ms），`-1` 关闭重排恢复逐包直注；**窗口保存后立即生效** |
| `kvm_speed_percent` | 100 | 本机作主控时速度微调倍率百分比（100=自动计算的标准速度；可在 50~300% 个性化微调）；**窗口保存后立即生效** |
| `kvm_edge_dead_zone` | 48 | 边缘死区高度（逻辑像素，按各屏缩放比换算）：屏幕上下两端这么大范围不触发跨屏/切回（关闭按钮、显示桌面、任务栏热区）；`0` 关闭死区；**窗口保存后立即生效** |
| `kvm_touchpad_gestures` | 关（false） | **实验性**：触控板双指滚动跨屏识别（接管期间转发，本机窗口仍会滚动；详见 `docs/touchpad-gesture-rawinput-plan.md`） |
| `kvm_touchpad_speed` | 100 | 触控板双指滚动的输出倍率百分比（100=基准；越大越快；**窗口保存后立即生效**） |

另有一个非手工编辑文件：`~/.copywhere/peers.json` 保存配对令牌列表（窗口自动维护）。

## 鼠标键盘跨屏（KVM，实验性）

推荐直接在窗口「屏幕布局」里把设备卡片拖到本机左右两侧：布局即时生效，
并**自动同步到对端**（A 配置右邻为 B ⟺ B 自动配置左邻为 A）；取消某侧（✕）或
换成其他设备时同样自动同步，旧邻居不会残留过期配置。

- 在 A 上把光标推向**右边缘并继续推**（累计约 8px，轻推即过）→ 控制权切到 B，键盘/滚轮同步转发；
  在 B 上把光标推回**左边缘并继续推**（累计约 8px）→ 交还 A；200ms 防抖消除卡顿，往返穿梭流畅自如
- **边缘死区（防误触）**：屏幕**上下两端**各 48 逻辑像素（按各屏缩放比换算）不触发跨屏/切回——
  那里是右上角窗口「关闭」按钮、右下角「显示桌面」和任务栏的地盘，把光标顶过去是日常操作，
  不该跳机；跨屏请用边缘**中段**。死区里顶边不累积、不会被"带"到中段触发，被控入口点也会
  自动避开死区。窗口「设置 → 跨屏控制」可调（0 = 关闭），保存即生效
- **被控期间在本机动一下鼠标或键盘 → 会话自动结束**（真人回到本机操作）
- 切换/切回只在**暴露边缘**（该侧没有相邻本地显示器）触发，双屏内侧边界不会误判；
  被控入口固定在主显示器（`kvm_entry_monitor` 可指定入口屏）
- **跨分辨率与实际 DPI 缩放自适应**：系统自动读取两端真实物理分辨率与 Windows 实际设置缩放比（如 ThinkBook 2880×1800 @ 200%），按逻辑视口换算科学基准增益并结合亚像素余数结转，解决高分缩放屏变慢问题，绝不随手速漂移；被控多屏自由漫游；窗口支持速度微调（默认 100%）
- 主控机失联 5 秒，副机自动释放控制；双向 ping/pong 保活
- 紧急退出：`Ctrl+Alt+Shift+X`

当前限制（MVP）：对 UAC/锁屏等安全桌面无法注入输入；按键按 VK 码转发，中文输入
依赖对端输入法（跨机传文字建议直接用剪贴板同步）；跨 ZeroTier 中继时延迟取决于
链路质量。协议细节见 [docs/protocol.md](docs/protocol.md)。

**已知问题：精确式触控板（PTP）双指滚动无法跨屏转发。**
主控端光标推向的边缘若恰好压在本机可滚动区域上（如列表/文档窗口），触控板双指
滚动会留在本机、副机收不到；改用外接鼠标/指点杆滚轮则完全正常。原因是 Windows
精确式触控板的双指滚动属于 `PT_GESTURE2` 指针手势，由系统直接投递给光标下窗口，
**不经过低级鼠标钩子（WH_MOUSE_LL），也不以滚轮形式出现在鼠标 Raw Input 流中**，
用户态全局钩子方案（本产品与 Synergy 等同类产品相同）原理上无法捕获。若触控板
厂商驱动提供"双指滚动输出为传统滚轮"选项，开启后可跨屏；否则请在外接输入设备上
滚动。边缘不可滚动的区域不受影响——那时手势会退回为滚轮消息，转发链路正常。

窗口「设置 → 跨屏控制」提供**实验性**的绕行方案：直接旁路监听触控板 HID 原始
触点、自研识别双指滚动并转发（默认关闭）。属于影子监听——接管期间本机窗口
仍会跟着滚，且部分机型不适用；实现与限制详见
[docs/touchpad-gesture-rawinput-plan.md](docs/touchpad-gesture-rawinput-plan.md)。

## 工作原理（简述）

- **节点身份**：节点 ID 由 Windows MachineGuid 派生的稳定指纹标识（重启不变）；
  进程每次启动生成随机会话号，对端据此识别节点重启并重置发送暂停状态。
- **发现**：UDP 广播公告（JSON，默认 2s 间隔），接收方以 ID 为键维护节点表；
  同一节点多网络来源的地址全部保留并按评分优选（`192.168.*` 恒优先，其次与本机
  同网段的直连地址），发送时逐地址尝试；超过 5 分钟无公告的地址才被剔除。
- **传输**：TCP 单行 JSON 头 + 原始负载 + JSON 应答；sha256 校验、临时文件原子改名、
  独立接收子目录；鉴权只认配对令牌（常数时间比较），配对经人工确认、应答沿原连接
  返回不可伪造。
- **右键发送**：菜单项是用户级静态 shell 动词
  （`HKCU\Software\Classes\{*,Directory}\shell\copywhere`，命令 `<exe> shell-send "%1"`），
  不做 COM 进程内扩展、不需管理员权限。每次点击唤起一个短进程 helper：它经**本机
  命名管道**（登录会话隔离）把路径交给运行中的窗口实例发送，成功后退出；实例用
  400ms 合并窗口把多选产生的多次调用拼成一次传输，对端只收到一个包。同一管道也用于
  二次启动唤回已有窗口，不再写 HTTP 面板地址/令牌文件。
- **剪贴板**：`AddClipboardFormatListener` 事件驱动（消息专用窗口收到
  `WM_CLIPBOARDUPDATE` 即唤醒，毫秒级），400ms 轮询序号作降级兜底；依次识别
  文件 → 文本（含 HTML/RTF 富格式一并搬运，对旧版本自动降级纯文本）→ 截图
  （PNG / CF_DIB 就地转 PNG），本进程写入跳过 + 接收后 3 秒冷却期双重防回环。
- **运行时诊断**：主循环节点、传输/KVM 计数、会话状态与环形日志汇聚为
  `diag.Snap` 诊断快照；watchdog 按各循环预算判定卡死并自动转储协程栈，
  便于定位"同步不动了"这类现场问题（详见 docs/protocol.md 第 5 节）。

完整的报文格式、配对时序、KVM 消息协议与容错行为见
[docs/protocol.md](docs/protocol.md)。

## 目录结构

```
cmd/copywhere/        CLI 入口（子命令、单实例互斥、托盘启动）
internal/app/         组装、发送/重试、zip 打包/自动解包、截图落盘
internal/clip/        Windows 剪贴板读写与事件监听（CF_HDROP / CF_UNICODETEXT /
                      HTML Format / RTF / PNG / CF_DIB）
internal/config/      配置
internal/diag/        运行时诊断（环形日志、事件计数、心跳 watchdog、栈转储）
internal/discovery/   UDP 广播发现与节点表（指纹、会话、多 IP 合并与优选、能力协商）
internal/monitor/     剪贴板监控（事件驱动 + 轮询兜底）与防回环
internal/transport/   TCP 传输协议（服务端/客户端）
internal/trust/       配对令牌信任库（peers.json，签发/校验/解除）
internal/kvm/         鼠标键盘跨屏（软 KVM）
internal/input/       全局输入捕获（钩子+Raw Input）与 SendInput 注入
internal/shellmenu/   资源管理器右键菜单注册（HKCU 静态 shell 动词，免管理员）
internal/ipc/         本机命名管道 IPC（右键发送、二次启动唤回窗口）
internal/record/      收发记录视图（核心与桌面窗口共用）
internal/panel/       共享层：事件总线、配置应用、图标与 Shell 辅助
internal/wailsui/     Wails v3 桌面窗口（Go 绑定 + 托盘 + 内嵌前端）
internal/bytesize/    字节数格式化
tools/icon/           图标生成脚本（应用图标、窗口图标）
docs/                 协议与设计说明、发布流程等
```

## 文档

- [docs/protocol.md](docs/protocol.md) — 发现 / 传输 / 配对 / KVM 协议与设计细节
- [docs/wails-ui.md](docs/wails-ui.md) — Wails 桌面窗口设计、验证与已知限制
- [docs/release.md](docs/release.md) — 版本发布流程
- [CHANGELOG.md](CHANGELOG.md) — 更新日志

## 参与贡献

欢迎 Issue 与 PR。提交前请通过本地质量门禁：

```powershell
go build ./...
go test ./...
go vet -unsafeptr=false ./...
gofmt -l .   # 输出应为空
```

新版本发布流程（CHANGELOG 定稿、构建、打 tag、GitHub Release）见
[docs/release.md](docs/release.md)。

## 已知限制

- 仅支持 Windows（剪贴板与输入注入使用 Win32 API）；协议与发现层是跨平台的。
- **富文本同步风险**：开启文本同步时，对端的 HTML/RTF 会被写回本机剪贴板，
  Word/浏览器粘贴时会解析这些格式。copywhere 对写回内容做魔数轻校验（不符即
  丢弃该格式），但仍建议**只与可信设备配对**——配对令牌 + 人工确认是唯一准入
  边界。不确定时可在窗口设置里关闭「文本同步」。
- 自动发现依赖 UDP 广播：跨路由/跨 VLAN 或开启了 AP 隔离的网段无法互相发现
  （可手动 `send`）。
- `run` 需要在交互式用户会话中运行（写剪贴板的要求），不适合作为系统服务。
- 接收端自动回贴的是"单个文件"、"解包后的文件夹/多文件"、"原始 zip 包"或"截图文件"。
- 超过阈值的剪贴板复制不会自动同步（日志会提示），需要时用 `send` 或窗口拖入手动发。
- 精确式触控板（PTP）双指滚动无法跨屏转发（`PT_GESTURE2` 不经钩子/Raw Input），
  详见上文「鼠标键盘跨屏（KVM）」章节；外接鼠标/指点杆滚轮不受影响。
  窗口提供默认关闭的实验性识别开关（影子监听，本机仍会滚动）。
- **右键发送依赖桌面窗口实例**：helper 通过命名管道联系运行中的窗口；终端 `run`
  模式不提供窗口 IPC，右键会提示先以窗口模式启动。升级 exe 后需重启进程，
  旧实例不认识新接口（会提示重启）。
- **首次运行可能被 SmartScreen 拦一次**：发布包未做代码签名，Windows 会提示
  「无法识别的应用」。点「更多信息 → 仍要运行」即可；也可在文件属性里勾选
  「解除锁定」。
- **托盘图标默认在折叠区**（Windows 11 默认策略）：点任务栏 `^` 把图标拖到任务栏，
  或在 设置 → 个性化 → 任务栏 → 其他系统托盘图标 里打开。
- **桌面窗口日志**：`~/.copywhere/gui.log`（双击启动时控制台被立即隐藏，这是唯一能
  看到启动过程与错误的地方；启动版本、崩溃堆栈都写在这里；超过 4MB 清空重来）。
- **提权/低完整性环境**：低完整性进程写不了用户目录、托盘图标会被丢弃、输入注入
  会被拒，窗口会明确拒绝启动并说明原因；提权环境下 shell 菜单/注入行为也受系统
  策略限制。`copywhere doctor` 会显示完整性级别与端口占用者。
- **双击后"什么都没发生"先看日志**：`~/.copywhere/gui.log` 最后一行就是失败点。
  常见是端口被占用（占用者会写进日志，也可能是别的会话/权限下的实例），
  或 WebView2 运行时缺失（窗口无法创建）。
- **看不清现场就跑自检**：`copywhere doctor` 会列出权限、配置、单实例锁、IPC、
  WebView2 版本、KVM/传输端口、右键菜单指向、通知区域登记与 `gui.log` 尾部，
  且全程只读。

## 许可证

[MIT](LICENSE)
