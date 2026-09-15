# copywhere

<p align="left">
  <img src="internal/webui/favicon.png" width="72" alt="copywhere 图标">
</p>

**局域网剪贴板 / 文件自动同步 + 鼠标键盘跨屏工具**（Windows，Go 实现，单 exe、零外部依赖）。

- **剪贴板同步**：在一台机器上 **Ctrl+C** 复制小于阈值的文件（默认 10MB）、文本或截图，
  局域网内所有在线 `copywhere` 节点会自动收到：文件落盘到接收目录并写回其剪贴板，
  对端直接 **Ctrl+V** 即可粘贴。
- **鼠键跨屏（软 KVM）**：把光标推向屏幕边缘即可用本机鼠标键盘控制相邻机器，
  键盘/滚轮同步转发，多机布局在面板拖动配置、自动互相同步。

无需配置 IP、无需账号、无需中转服务器——数据只在你的机器之间点对点直传。

![Platform](https://img.shields.io/badge/platform-Windows-blue) ![Go](https://img.shields.io/badge/Go-1.21%2B-00ADD8) ![License](https://img.shields.io/badge/license-MIT-green)

## 特性一览

- **节点自动发现**：UDP 广播（各网段定向广播 + 回环），无需配置 IP，节点上下线自动感知
- **配对认证**：面板点击「配对」→ 对端「接受配对」即完成；双方交换逐对独立的配对
  令牌（`~/.copywhere/peers.json`，可单独解除），未配对的节点无法传输
- **图形界面（推荐）**：双击 exe 即得——系统托盘 + 浏览器控制面板，对话式传输工作台、
  全局拖拽投送、屏幕布局（KVM）、Ctrl+K 命令面板、深浅双主题，SSE 实时刷新，
  仅监听 `127.0.0.1`
- **剪贴板全类型同步**：文件（CF_HDROP）、文本、截图（Win+Shift+S / PrintScreen
  直接写入剪贴板的图片）自动同步；接收端自动回贴
- **多文件 / 文件夹**：自动打包 zip 传输，对端**自动解包**后回贴原内容（文件夹仍是
  文件夹、多文件仍是多文件）；用户亲手发送的 zip 不误解包
- **手动发送**：`copywhere send` 或面板拖入文件不受阈值限制（单文件上限 4GB）
- **可靠传输**：TCP + sha256 校验 + 临时文件原子改名；每次接收落在独立子目录，
  文件保留原始文件名
- **多网卡地址合并**：同一节点从多个网络（局域网 + ZeroTier 等）广播只算一个节点，
  全部地址保留并优选排序，发送时逐地址尝试，任一可达即成功
- **慢链路友好**：超时预算按 256KB/s 估算（ZeroTier 中继等慢链路不误杀），失败原因记录在对端日志
- **容错**：对端短暂离线时内容暂存 60 秒自动重试；配对被拒后提示一次即暂停、不高频重试，
  完成配对后自动恢复；剪贴板序号 + 冷却期双重防回环
- **鼠标键盘跨屏（实验性）**：光标推向显示器"暴露边缘"并继续推动即可控制相邻机器，
  键盘/滚轮同步，布局在面板拖动配置并自动同步到对端；`Ctrl+Alt+Shift+X` 紧急退出
- **三页终端界面**：`run` 纯终端常驻方式（适合服务器 / SSH），默认显示 1 日志 /
  2 在线节点 / 3 文件记录三个标签页

## 安装

### 下载预编译 exe

从 [GitHub Releases](../../releases) 下载 `copywhere-vX.Y.Z-windows-amd64.exe`，
双击即可运行——首次启动自动生成默认配置（节点名默认为主机名，可在面板「设置」修改）。

### 从源码构建

要求 Go 1.21+（Windows）。

```powershell
git clone <本仓库>
cd copywhere

# 开发构建（-H windowsgui 确保双击启动时不弹出控制台黑框）
go build -ldflags "-H windowsgui" -o copywhere.exe ./cmd/copywhere

# 发布构建（注入版本号，与 CHANGELOG / git tag 保持一致）
$v = "v0.3.0"
go build -trimpath -ldflags "-s -w -H windowsgui -X copywhere/internal/app.Version=$v" `
  -o copywhere.exe ./cmd/copywhere

go test ./...        # 运行全部单元测试
copywhere version    # 查看版本号
```

exe 图标与版本信息由 `cmd/copywhere/rsrc_windows_amd64.syso` 提供（`go build` 自动嵌入）；
重新生成方式见 [docs/release.md](docs/release.md)。

## 快速开始（两台机器 A、B）

1. **启动**：两台机器上分别**双击 `copywhere.exe`**——托盘常驻，浏览器自动打开控制面板。
   首次运行 Windows 会弹防火墙询问，选择「允许」（至少专用网络）——发现/传输/KVM
   三个端口（默认 47830~47832）需要允许入站。

2. **验证发现**：面板左侧设备坞应出现对方的设备卡片（含全部地址与活跃时间）。

3. **配对**：点击对方节点卡片上的「配对」，对端面板弹出「接受配对」确认后即完成。
   未配对的节点无法传输。

4. **使用**：在 A 上复制一个小文件（或把文件直接拖进面板窗口）→ B 的面板工作台出现
   消息气泡，在 B 上直接 **Ctrl+V** 即可粘贴；复制文本、按 Win+Shift+S 截图同理。

## 使用指南

### 图形界面（`gui`，推荐）

双击 `copywhere.exe` 或执行 `copywhere gui`，同一进程内启动完整服务
（发现/传输/剪贴板/KVM）+ 系统托盘 + 浏览器控制面板（默认
`http://127.0.0.1:47890`，端口被占用时自动换用随机端口）。

面板为单屏工作台范式：

- **工作台**（主页）：中央为对话式传输时间线——左收右发，文本直接显示内容
  （点击展开、可一键复制回剪贴板），文件显示类型徽标卡片与大小，接收目录内的图片
  显示缩略图（点击放大预览、双击打开文件）；进行中的收发均有实时进度条，收到的
  文件/文本**双击直达**（打开文件 / 复制内容）；记录按 今天/昨天/日期 分组，本地
  持久化最近 200 条，刷新面板不丢失
- **发送**：底部输入框回车即发文本（Shift+Enter 换行），附件按钮选择文件，
  或把文件拖进窗口任意位置（均不受阈值限制，上传可取消）；重复复制的内容自动
  合并计数（×N）
- **设备坞**：左侧常驻本机与在线节点卡片（全部地址、活跃时间、配对状态），
  「配对 / 设为左右邻」直接在卡片上完成；配对请求弹出确认框（不响应 Esc，须明确裁决）
- **屏幕布局**（KVM）：仿 Windows 多显示器排列，设备渲染为显示器卡片，拖到本机
  左侧/右侧空位即配置邻居，配置即时生效并**自动同步到对端**（互为镜像）
- **运行日志**：底部终端风格抽屉（带未读角标，错误行着色）
- **Ctrl+K 命令面板**：发送、暂停同步、跨屏开关、切换主题、解除配对等全部操作键盘直达；
  `Ctrl+1` / `Ctrl+2` 切换工作台 / 屏幕布局
- **设置**：节点名 / 阈值 / 接收目录 / 自动回贴 / 文本同步 / 已配对设备管理，
  所有修改**立即保存并即时生效**，无需重启（KVM 开关热启停，节点名下一次广播即同步）

**系统托盘**右键菜单：打开面板 · 暂停/恢复自动同步 · 打开接收目录 · 退出。

**安全**：面板仅监听 `127.0.0.1`，首次经托盘 URL 携带随机访问令牌并种下 cookie；
面板地址同时写入 `~/.copywhere/panel_url.txt` 备查。`gui` 与 `run` 通过命名互斥锁互斥。

### 终端方式（`run`）

适合服务器 / SSH 等无托盘环境。`run` 默认启用三页终端界面，`-plain` 或输出非终端时
退化为纯日志。快捷键：

| 按键 | 功能 |
| --- | --- |
| `1` / `2` / `3`、`Tab`、`←` `→` | 切换 日志 / 在线节点 / 文件记录 |
| `↑` `↓`、`PgUp` `PgDn` | 滚动日志与文件记录（历史最多保留 1000 行） |
| `q` / `Esc` / `Ctrl+C` | 退出 |

### 命令

| 命令 | 说明 |
| --- | --- |
| `copywhere`（无参数，或**直接双击 exe**） | 进入 `gui` 模式；双击启动时自动隐藏控制台窗口，仅托盘常驻 |
| `copywhere init` | 生成默认配置 |
| `copywhere run [-config 路径] [-plain]` | 启动常驻服务；默认三页 TUI，`-plain` 纯日志 |
| `copywhere gui [-config 路径]` | 启动服务 + 系统托盘 + 浏览器控制面板（与 `run` 互斥） |
| `copywhere nodes [-config 路径] [-wait 4s]` | 扫描并列出在线节点（含多网卡地址合并结果） |
| `copywhere send [-config 路径] [-text 内容] [-wait 4s] 文件/目录...` | 立即发送到所有在线节点，不受阈值限制 |
| `copywhere clip-test` | 本机剪贴板读写自检（覆盖并尝试恢复剪贴板） |
| `copywhere version` | 显示版本号 |

`copywhere help` 原文：

```text
copywhere — 局域网剪贴板/文件自动同步工具

用法:
  copywhere init                            生成默认配置
  copywhere run [-config 路径] [-plain]     启动服务（节点发现 + 传输 + 剪贴板监控）
  copywhere gui [-config 路径]              启动服务 + 系统托盘 + 浏览器控制面板
  copywhere nodes [-config 路径] [-wait 4s] 扫描并列出局域网在线节点
  copywhere send [-config 路径] [-text 内容] [-wait 4s] 文件/目录...
                                            立即发送到所有在线节点（不受大小阈值限制）
  copywhere clip-test                       本机剪贴板读写自检
  copywhere version                         显示版本号

工作方式:
  - 直接双击 copywhere.exe = gui 模式（隐藏控制台窗口，托盘常驻 +
    浏览器自动打开控制面板；命令行终端里执行时不隐藏，便于查看日志）
  - 各节点通过 UDP 广播自动发现彼此，无需配置 IP；同一节点多网卡地址自动去重
  - 配对认证：在 gui 面板「在线节点」点击「配对」，对端在面板确认后即完成，
    双方交换独立配对令牌（peers.json），可随时解除配对；
    未配对的节点无法传输（共享 token 认证已废弃）
  - run 运行期间，复制（Ctrl+C）小于阈值的文件或文本，会自动推送到所有在线节点
  - 对端收到文件后保存到接收目录并自动写回其剪贴板，直接 Ctrl+V 即可
  - run 默认启用三页终端界面（1 日志 / 2 在线节点 / 3 文件记录），
    -plain 或输出非终端时退化为纯日志
  - gui 同一进程内启动服务与系统托盘，并在浏览器打开控制面板
    （传输动态/节点/日志/设置），面板只监听 127.0.0.1

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
| `announce_interval_sec` | 2 | 广播间隔 |
| `peer_ttl_sec` | 12 | 节点存活判定时长（约为广播间隔的 5~6 倍） |
| `kvm_enabled` | true | 鼠标键盘跨屏开关（面板内热启停） |
| `kvm_port` | 47832 | KVM 监听端口 |
| `kvm_left` / `kvm_right` | 空 | 左/右边缘邻居的**节点名**（可在面板「屏幕布局」拖动配置，自动同步到对端） |
| `kvm_entry_monitor` | -1 | 被控入口显示器下标（`-1` = 恒用主显示器） |
| `kvm_move_interval_ms` | 0（=8） | 主控移动合拍间隔（ms），越小越跟手但更耗带宽；**面板保存后立即生效** |
| `kvm_reflow_step_ms` | 0（=4） | 被控重排注入节拍（ms），`-1` 关闭重排恢复逐包直注；**面板保存后立即生效** |
| `kvm_speed_percent` | 100 | 本机作主控时发送位移的手调系数百分比（100=不补偿；越大远程越快）。A→B 偏慢调高 A、B→A 偏慢调高 B；**面板保存后立即生效** |
| `kvm_touchpad_gestures` | 关（false） | **实验性**：触控板双指滚动跨屏识别（接管期间转发，本机窗口仍会滚动；详见 `docs/touchpad-gesture-rawinput-plan.md`） |
| `kvm_touchpad_speed` | 100 | 触控板双指滚动的输出倍率百分比（100=基准；越大越快；**面板保存后立即生效**） |
| `web_port` | 47890 | GUI 面板端口（`gui` 命令）；负数 = 随机端口 |

另有一个非手工编辑文件：`~/.copywhere/peers.json` 保存配对令牌列表（面板自动维护）。

## 鼠标键盘跨屏（KVM，实验性）

推荐直接在面板「屏幕布局」里把设备卡片拖到本机左右两侧：布局即时生效，
并**自动同步到对端**（A 配置右邻为 B ⟺ B 自动配置左邻为 A）；取消某侧（✕）或
换成其他设备时同样自动同步，旧邻居不会残留过期配置。

- 在 A 上把光标推向**右边缘并继续推**（累计约 16px）→ 控制权切到 B，键盘/滚轮同步转发；
  在 B 上把光标推回**左边缘并继续推** → 交还 A
- **被控期间在本机动一下鼠标或键盘 → 会话自动结束**（真人回到本机操作）
- 切换/切回只在**暴露边缘**（该侧没有相邻本地显示器）触发，双屏内侧边界不会误判；
  被控入口固定在主显示器（`kvm_entry_monitor` 可指定入口屏）
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

面板「设置 → 跨屏控制」提供**实验性**的绕行方案：直接旁路监听触控板 HID 原始
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
- **剪贴板**：400ms 轮询序号变化，依次识别 文件 → 文本 → 截图（PNG / CF_DIB 就地
  转 PNG），本进程写入跳过 + 接收后 3 秒冷却期双重防回环。

完整的报文格式、配对时序、KVM 消息协议与容错行为见
[docs/protocol.md](docs/protocol.md)。

## 目录结构

```
cmd/copywhere/        CLI 入口（子命令、单实例互斥、托盘启动）
internal/app/         组装、发送/重试、zip 打包/自动解包、截图落盘
internal/clip/        Windows 剪贴板读写（CF_HDROP / CF_UNICODETEXT / PNG / CF_DIB）
internal/config/      配置
internal/discovery/   UDP 广播发现与节点表（指纹、会话、多 IP 合并与优选）
internal/monitor/     剪贴板轮询监控与防回环
internal/transport/   TCP 传输协议（服务端/客户端）
internal/trust/       配对令牌信任库（peers.json，签发/校验/解除）
internal/kvm/         鼠标键盘跨屏（软 KVM）
internal/input/       全局输入捕获（钩子+Raw Input）与 SendInput 注入
internal/ui/          三页终端界面（日志/在线节点/文件记录）
internal/webui/       GUI：事件总线、本地 HTTP 面板（REST+SSE）、系统托盘
internal/bytesize/    字节数格式化
tools/icon/           图标生成脚本（应用图标、托盘菜单图标）
docs/                 协议与设计说明、发布流程等
```

## 文档

- [docs/protocol.md](docs/protocol.md) — 发现 / 传输 / 配对 / KVM 协议与设计细节
- [docs/gui-plan.md](docs/gui-plan.md) — GUI 方案规划与落地记录
- [docs/webui-ux-audit.md](docs/webui-ux-audit.md) — WebUI 交互审计与修复状态
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
- 自动发现依赖 UDP 广播：跨路由/跨 VLAN 或开启了 AP 隔离的网段无法互相发现
  （可手动 `send`）。
- `run` 需要在交互式用户会话中运行（写剪贴板的要求），不适合作为系统服务。
- 接收端自动回贴的是"单个文件"、"解包后的文件夹/多文件"、"原始 zip 包"或"截图文件"。
- 超过阈值的剪贴板复制不会自动同步（日志会提示），需要时用 `send` 或面板拖入手动发。
- 精确式触控板（PTP）双指滚动无法跨屏转发（`PT_GESTURE2` 不经钩子/Raw Input），
  详见上文「鼠标键盘跨屏（KVM）」章节；外接鼠标/指点杆滚轮不受影响。
  面板提供默认关闭的实验性识别开关（影子监听，本机仍会滚动）。

## 许可证

[MIT](LICENSE)
