# MouseWithoutBorders（PowerToys）× copywhere 技术对比报告

> 对比对象
> - **MWB**：`PowerToys/src/modules/MouseWithoutBorders`（C# / .NET 8 / WinForms，约 22.7k LOC，4 个可执行体）
> - **copywhere**：本仓库（Go 1.21+，单 exe，约 7.8k LOC 主体 + 2.6k LOC 测试）
>
> 方法：MWB 侧精读 `SocketStuff.cs`(90KB) / `Common.cs`(62KB) / `Clipboard.cs`(50KB) /
> `MachineStuff.cs` / `InputHook.cs` / `InputSimulation.cs` / `Encryption.cs` / `DATA.cs` /
> `Package(Type).cs` / `Receiver.cs` / `DragDrop.cs` / `frmScreen.cs` / `Service/Worker.cs` /
> `ModuleInterface/dllmain.cpp` 等，交叉验证了三份独立分析报告；copywhere 侧通读全部核心包。
> 结论均为静态源码阅读所得，未运行 MWB。

---

## 0. 一句话定位差异

| | MWB | copywhere |
|---|---|---|
| 本质 | **软 KVM 优先**：鼠标键盘跨屏是第一公民，剪贴板/拖拽是附属 | **剪贴板/文件同步优先**：跨机 Ctrl+C→Ctrl+V 是第一公民，KVM 是实验性附加 |
| 产品形态 | PowerToys 的一个模块，依赖 runner + 服务 + helper 多进程 | 零依赖单 exe，双击即用 |
| 目标规模 | 固定 **4 台**机器矩阵 | 理论任意多节点（广播域内） |
| 信任建立 | 手工录入/自动生成**预共享密钥**（16 字符），同网段同密钥即成组 | **逐对配对令牌 + 人工 TOFU 确认**，未配对一律拒绝 |

这个根本差异决定了下面几乎所有技术选择的分岔。

---

## 1. 技术栈与进程模型

**MWB —— 多进程 + 高权限**
- `PowerToys.MouseWithoutBorders.exe`：主逻辑、tray、钩子宿主、TCP。服务模式被拉起**两份**（`winlogon` 安全桌面 + `default` 交互桌面）。
- `...Service.exe`：Windows 服务，`Worker.StartAsync` 用 `WTSQueryUserToken`+`WinLocalSystemSid`+`SetTokenInformation(TokenSessionId)`+`CreateProcessAsUser` 在两个桌面上各拉一份 app，**引导完即自停**（非常驻守护）。
- `...Helper.exe`：独立进程，专司剪贴板监听 + 拖拽取文件名（低完整性投递 / 安全桌面上下文需要）。
- `ModuleInterface/dllmain.cpp`：runner 集成、SCM 注册、`netsh advfirewall` 放行端口。
- 进程被设为 `ProcessPriorityClass.RealTime`，PerMonitorV2 DPI 感知。
- 线程繁多：UI / InputCallback(STA,Highest) / Helper / InputSimulation(串行注入) / Receiver / 每条 TCP 连接一个 STA 线程。

**copywhere —— 单进程多协程**
- 一个 `copywhere.exe`（`-H windowsgui`）内含：transport server、discovery、monitor、KVM、webui 全部以 goroutine 运行；`context` 统一生命周期。
- 唯一的"额外线程"是 `clip.clipboardThread`——**永久 `LockOSThread` 的专用剪贴板线程**，串行化所有 `OpenClipboard/CloseClipboard`（解决 Go 调度器迁移导致 Close 静默失败、剪贴板泄漏的经典坑）。
- 输入钩子在独立 OS 线程消息泵上运行（`input_windows.go`）。

**评价**：copywhere 用 Go 的 goroutine + context 把 MWB 需要 4 个进程 + 十几个命名线程 + Global Mutex 协调的东西收敛成单进程，工程复杂度和部署成本低一个数量级；代价是**放弃了 MWB 的锁屏/安全桌面能力**（无服务、无 SYSTEM 令牌跨桌面注入）。

---

## 2. 节点发现与拓扑

**MWB —— 无广播、手工矩阵、全双工常连接**
- **没有 UDP 自动发现**。机器靠 `MachineMatrix`（4 格，`frmMatrix` UI 或热键配置）+ `MachinePool`（名字↔ID↔IP 学习表）；解析优先 DNS（`Dns.GetHostEntry`），可用 `Name2IP` 手工映射覆盖，`BadIPs` 记录失败地址。
- 拓扑是**全连接 TCP 网状**：`UpdateTCPClients()` 对矩阵中每台机器发起 `TcpClient.Connect(ip, TcpPort+1)`（连对端**消息端口**），同时本机也在 `TcpPort`（剪贴板）/`TcpPort+1`（消息）`AcceptSocket`。默认端口 **15100 / 15101**（`MouseWithoutBordersProperties`）。每对机器之间通常维持两条 socket（一条我作 client、一条我作 server）。
- 连接是**长连接、状态机驱动**：`MainTCPRoutine` → `Handshake → Handshaking → Connected`，`TcpSk` 持有惰性创建的 `EncryptedStream/DecryptedStream`（`CryptoStream`），后续所有 32/64 字节包复用同一条加密流。
- 保活：每 60s 向 `ID.ALL` 广播 `Heartbeat`（`HelperTimer` 计数 `count%600`，100ms tick）。
- 断线重连：`WSAECONNRESET`/read-error → `PleaseReopenSocket` 标志位 → UI 线程重建；`REOPEN_WHEN_HOTKEY`（Ctrl+Alt+Shift+F5）手工触发。

**copywhere —— UDP 定向广播、自动成网、短连接按需**
- **UDP 广播自动发现**（各网段定向广播 + 127.0.0.1），2s 公告，12s TTL；节点 ID = `sha256(MachineGuid)` 稳定指纹，8 字节随机 session 判进程重启。
- **多网卡地址合并 + 优选排序**（同指纹多来源 IP 归一节点，`192.168.*` 恒优先于 ZeroTier 等虚拟网段），发送时逐地址尝试任一成功即成功。
- **无 KVM 长连**、传输是**每文件一条 TCP 短连接**（JSON 头 + 裸负载 + JSON 应答，收完即关）；仅 KVM 会话期维持一条 TCP。
- 端口规划：47830/udp 发现、47831/tcp 传输、47832/tcp KVM、47890 面板。

**评价**：这是两项目**最本质的架构分野**。
- copywhere 的"广播 + 稳定指纹 + 地址合并"是 MWB **完全没有**的能力，直接消灭了 MWB 最被诟病的"手工填机器名/IP、DNS 解析失败、DHCP 换 IP 就失联"痛点——**copywhere 在这一维度明显更优**。
- MWB 的**常连接网状 + 长握手加密流**是为"每毫秒都要发鼠标包"的低延迟硬需求服务的：一次 TCP+加密握手摊销到整个会话。copywhere 的短连接模型若照搬到 KVM 会致命（每包握手），所以 copywhere 让 KVM 单独用长连接、内容传输用短连接——**这个分层是对的**。

---

## 3. 协议与数据格式

**MWB —— 定长二进制包 + union 结构**
- `DATA`：`[StructLayout(LayoutKind.Explicit)]` 的 C 风格 union，`PackageType/Id/Src/Des/DateTime` + 叠加 `KEYBDDATA/MOUSEDATA/MachineName/4×ID`；定长 **32 字节**（`PACKAGE_SIZE`）或 **64 字节**（`PACKAGE_SIZE_EX`，鼠标/键盘/心跳/剪贴板/矩阵等"大"包）。
- `PackageType` 枚举（Hi/Hello/ByeBye/Heartbeat(_ex/l2/l3)/Awake/HideMouse/Clipboard*/Keyboard/Mouse/Handshake(Ack)/Matrix/NextMachine/MachineSwitched…），**整型线上编码**。
- 粘包靠**定长读取**（`TcpReceiveData` 精确收 32/64 字节）；无长度前缀、无分隔符。
- 每包字节层认证：`bytes[3..2]=magic(24bit哈希的16位)` + `bytes[1]=Σbytes[2..]` 单字节校验和；连续 10 个非法包判定密钥不符。
- **包级幂等去重**：`Receiver.PreProcess` 用 50 槽环形数组按 `package.Id`（进程内自增、持久化）丢弃广播路径重复包。

**copywhere —— 单行 JSON，人类可读**
- 传输头/应答、KVM 消息、发现公告**全是 JSON**，`\n` 行分隔；靠 `size` 字段界定裸负载边界。
- 版本字段 `v:1`；未知字段被 `encoding/json` 忽略（但文档诚实记录了 `kvm.clear` 这种"新增语义字段"在混合版本下会**误解**的破坏性）。
- 结构体即 `transport.Header`/`Response`/`wireMsg`，Go tag 直接映射。

**评价**：
- MWB 定长二进制是 2008 年 C/C++ 时代的遗产（`SuppressMessage CA1900 ValueTypeFieldsShouldBePortable` 是活化石）。在**每秒上百个 4 字节增量鼠标包**的场景，定长二进制省掉的带宽/序列化开销有意义。
- copywhere 的 JSON 在剪贴板/文件这种"低频大负载"场景完全够用，且可调试性碾压。
- **唯一值得 copywhere 认真考虑的二进制化理由**是 KVM：`{"t":"move","dx":12,"dy":-3}` 一行 JSON ≈ 25 字节，而固定结构二进制只需 8~12 字节，在 125Hz 合拍下是 ~2x 的报文压缩。但绝对带宽仍极小（3KB/s vs 1.5KB/s），**收益不抵复杂度，不建议为 KVM 上二进制**。

---

## 4. 传输可靠性与流控

| 维度 | MWB | copywhere |
|---|---|---|
| 完整性校验 | 明文层 magic + 单字节校验和 + AES-CBC 块对齐 | **sha256 全负载校验**（强得多） |
| 落盘原子性 | `.{Guid}.partial` + `File.Move(overwrite)` | `.cw-partial-*` + 原子改名，**每次接收独立子目录** |
| 断点/续传 | 无 | 无（但有慢链路超时预算） |
| 超时策略 | `SendTimeout=500ms`、`CONNECT_TIMEOUT=60s`、剪贴板握手 30s | **按 256KB/s 动态预算** `60s+size/256KB`，ZeroTier 慢链路不误杀 |
| 发送失败补偿 | 无应用层重试（靠 socket 重连） | **60s 暂存重试队列**（`flushLoop`），对端上线即补发 |
| 进度反馈 | 仅 tray 文本 tooltip | **双向 256KB 粒度进度** → 面板实时进度条 |
| 大文件上限 | 剪贴板文件 100MB；拖拽不限；文本 20MB/图 50MB | 单文件 4GB、文本 4MB；剪贴板自动阈值默认 10MB |

**评价**：copywhere 的可靠性工程（sha256、原子落盘、动态超时预算、重试队列、双向进度）**全面优于 MWB**。MWB 的校验强度（2^8~2^17 的伪造检出）在现代标准下是**明显脆弱点**。唯一 copywhere 不如 MWB 的是**没有断点续传**——但两者都不做分块续传，MWB 甚至更差（大文件走单条 pull 连接，中断即重来）。

---

## 5. 安全与鉴权模型（差异最大的维度之一）

**MWB —— 预共享密钥、对称加密每条消息、但信任模型弱**
- **无 TLS、无证书**（全仓 `X509|Certificate` 零命中）。用户手工同步一枚 **16 字符预共享密钥**（`CreateRandomKey`，排除易混字符），DPAPI-CurrentUser 加密存本地，**从不过网**。
- 每条 TCP 流：明文交换 16B salt + 16B IV → `PBKDF2-SHA512(key, salt, 100k)→AES-256` → **AES-256-CBC（PaddingMode.Zeros）加密后续每一条消息**。每连接重新派生密钥（per-connection salt，阻断跨连接彩虹表；per-connection IV）。
- 活体/对偶证明：`Handshake` 随机数取反回显；`MagicNumber=Get24BitHash(key)`（50000 轮 SHA-512）做包级"密钥一致"检查。
- **信任模型弱点**：`ShakeHand` 用"是否已连到该 name + MachinePool 能解析 ID"代替认证；**同网段任何持同一密钥的机器即自动成组**，无逐对手工确认、无主机指纹绑定；密钥以 DPAPI-CurrentUser 落盘，本机恶意软件可直读。

**copywhere —— 明文传输 + 逐对令牌 + 人工 TOFU**
- **传输内容不加密**（仅 LAN 假设 + 令牌准入）；文件靠 sha256 保证完整性但不保证机密性。
- **逐对独立配对令牌**（`trust.go`，`peers.json`）：面板点击配对 → 对端**人工确认弹窗**（150s、不响应 Esc、须明确裁决）→ 双向交换各自签发的令牌；`sender_id` 须在信任库且 token 常数时间比对相等。未配对一律拒绝。
- 单独解除配对即删除记录，**立即双向失效**；config 共享 token 已彻底废弃。
- KVM 复用同一配对令牌体系（`hello` 携带令牌，被控端按 `id` 查表比对）。

**评价**：这是**各有硬伤**的维度，且两者的弱点正好互补——

| 攻击面 | MWB | copywhere |
|---|---|---|
| 链路窃听 | ✅ 全量 AES 加密（机密性有） | ❌ **明文，LAN 内可被抓包读取剪贴板/文件内容** |
| 密文篡改 | ❌ CBC+Zeros **无 MAC/AEAD**，可位翻转 | 无加密故无此问题；但明文同样可篡改 |
| 伪造节点 | ❌ 持密钥即自动加入，无主机绑定（密钥泄露=全网沦陷） | ✅ 逐对手工 TOFU + 稳定指纹，MITM 难 |
| 密钥/令牌强度 | ❌ 16 字符低熵口令 + magic 泄露 key 指纹 | ✅ 32 hex 随机令牌 |
| 细粒度吊销 | ❌ 换密钥=全组重配 | ✅ 单节点解除配对 |

> copywhere 的**准入模型（逐对 TOFU + 稳定指纹 + 单点吊销）是 MWB 该学却学不来的现代设计**；
> 但 copywhere **明文传输**是相对 MWB 的**实质倒退**——在 ZeroTier/跨网段/共享 WiFi 场景下，
> "数据只在你的机器之间点对点直传"的承诺没有密码学背书。

---

## 6. 剪贴板同步

**MWB**（详见分析）：
- **事件驱动**：helper 进程 `AddClipboardFormatListener`(→`WM_CLIPBOARDUPDATE`)，失败回落 `SetClipboardViewer`(→`WM_DRAWCLIPBOARD`)。**无轮询**。
- 格式：Text / RTF / HTML 用 GUID 分隔符打包共存；Image→PNG；**文件只取 `files[0]`（单文件，目录拒绝）**。
- 双通道：≤1MB 即时 TCP 48B/包推流；>1MB 只广播 `Clipboard` 节拍照，**切机时才 pull**（`GetRemoteClipboard`），拉不通时 `ClipboardAsk` 让对端反向 push。
- 防回环三重：内容比对 + `HasSwitchedMachineSinceLastCopy` + 1s 时间闸门。
- 无剪贴板历史（网传"10 条"实为 `SetDataObject` 重试次数与 50 槽去重队列的误读）。

**copywhere**：
- **轮询驱动**：400ms `GetClipboardSequenceNumber`；变化即按 **文件→文本→截图** 优先级读取。
- 格式：CF_HDROP（**多文件/目录原生支持**）、CF_UNICODETEXT、截图（优先应用放的 "PNG" 格式，否则 CF_DIB→PNG）。带预览位图的文本按文本处理，不误判截图。
- 多文件/目录**自动打包 zip 传输 + 对端自动解包回贴**（`bundle=true` 标记区分合成包 vs 用户亲手发的 zip）——**MWB 做不到目录**。
- 防回环双重：`OwnSeq` 序号 + 收后 3s 冷却。
- 无 RTF/HTML 富格式；无剪贴板历史。

**评价**：
- copywhere 的**文件类型覆盖更全**（多文件、目录、zip bundle 往返无损），这是相对 MWB 的**明确功能优势**——MWB 拖文件遇到目录只会提示"zip it first"。
- MWB 的**事件驱动检测**比 copywhere 的 400ms 轮询更省电、更实时（无感延迟），且它的**"小推/大拉/反向 push 回退"三段式**是为不同带宽/防火墙环境设计的巧思。
- copywhere 缺 RTF/HTML 保真：从 Word 复制带格式文本到对端只会得到纯文本。

---

## 7. 文件拖拽（Drag & Drop）

**MWB —— 有完整 DnD 跨屏**：`InputHook` 检测按住拖动越过边缘 → 12 步状态机（`DragDropStep01~12`）→ 把 helper 窗口"瞬移逼出 `DragEnter`"问出资源管理器正在拖的文件名 → `InputSimulation.MouseUp()` 合成终止源端拖拽 → 目标机落 `Desktop\MouseWithoutBorders\` 并打开。**单文件**，跨第二台机器有 `ChangeDropMachine` 接力。用 70×70 半透明主窗当拖拽"影像"。

**copywhere —— 无系统级拖拽跨屏**。全局拖拽投送只在**自家 web 面板内**（把文件拖进浏览器窗口，走手动发送通道，不受阈值限制），不是 Windows 资源管理器的原生拖拽。

**评价**：**这是 copywhere 相对 MWB 唯一"整块缺失"的重量级功能**。真实资源管理器拖拽跨屏的 UX 价值很高（直接把文件"甩"到对面屏幕），但它极其脏、竞态多（`FindWindow`+20×20ms 轮询逼 DragEnter、合成 MouseUp、完整性边界），是 MWB 的**高维护成本反面教材**。对 copywhere 是"诱人但危险"的借鉴项，见第 12 节。

---

## 8. 鼠标键盘跨屏（KVM）

| 子项 | MWB | copywhere |
|---|---|---|
| 边缘切换判定 | **单帧越界即切** + `SKIP_PIXELS=1` 边缘带 + 100ms `lastJump` 去抖 + 四角 100px 禁区；**无 push 累积** | **push 累积模型**：贴边继续推，Raw 位移累计 ≥16px 才切；切回需先"武装"（深入 ≥30px）再推 ≥16px |
| 坐标 | **Universal 0..65535 归一化** + `MOUSEEVENTF.ABSOLUTE` 仿射，吸收多屏/DPI/负原点；`XY_BY_PIXEL=300000`/`MOVE_MOUSE_RELATIVE=100000` 哨兵编码复用单字段 | 相对位移（Raw Input）为主，入口用 `SetCursorPos` 绝对定位（SendInput 归一化在双屏异缩放下基准错位） |
| 注入 API | SendInput(64) 为主，**纯 MOVE 优先 WinRT `InputInjector`**；`INPUT64` 对齐修复 | SendInput；移动走相对注入 |
| 移动节流 | 每物理事件即发（钩子线程同步写流） | **8ms 合拍(≈125Hz)** 累计位移再发 + 被控端 **4ms reflow(≈250Hz)** 平滑排空 |
| 键映射 | 线上只传 **VK**，收端 `MapVirtualKey` 现算 scan code；`LLKHF.EXTENDED→KEYEVENTF.EXTENDEDKEY` | 传 vk+scan+ext，收端按扩展键标志注入 |
| 链式多机切换 | **`NextMachine` 接力**：相对模式下受控端本地检测边缘 → 向主控发 `NextMachine(host,next,xy)` → 由主控执行 B→C | 两端布局镜像互推（`SyncKVMFrom`），但**会话是 1 对 1**（进入副机后回到主控本机布局自然跨越，不做 A→B→C 远程接力） |
| 速度一致性 | 靠 universal 绝对坐标天然对齐 | 自动折算模型**已被实测证伪**，改为每端**手调 `kvm_speed_percent`** |
| 注入回声区分 | `RealData` 标志（**竞态 hack**，非 `LLMHF_INJECTED`） | 检查 `LLMHF_INJECTED`/`LSM_STATE_INJECT`（正确做法） |
| 真人让位 | 拓扑天然（各自钩子只在控制远端时吞本地）+ `ReleaseAllKeys` 防粘滞键 + `HideMouse` | `OnLocalActivity` 检测物理输入结束被控会话 |
| 紧急退出 | 各种热键 + `HotKeyExitMM` | `Ctrl+Alt+Shift+X` |
| 触控板 | 未特别处理 | **自研 PTP HID Raw Input 双指滚动识别**（实验性，PoC） |

**评价**：
- **切换哲学相反**：MWB"碰到就切"上手快但易误触（快速甩动能一跳两台）；copywhere"push 累积 + 武装切回"更可控、防误触，是 Synergy/Windows11 新版的现代做法。copywhere 在此**更稳健**。
- MWB 的 **Universal 65535 坐标 + `NextMachine` 链式切换**是两个真正的精华（第 12 节重点）。copywhere 的手调速度系数暴露了相对位移模型的固有缺陷——**根因正是没用 MWB 的绝对归一化坐标**。
- copywhere 用 `LLMHF_INJECTED` 区分注入回声，**修正了 MWB 的 `RealData` 竞态 bug**。

---

## 9. 桌面 / 权限 / 服务

**MWB**：完整支持锁屏、登录界面、屏保桌面——服务以 SYSTEM 在 `Winlogon\` 和 `Default\` 两桌面各起一份 app，`Global\` 命名 Mutex 协调监听权，`CheckForDesktopSwitchEvent` 在非活跃桌面自杀/请服务补拉，`NotPhysicalConsoleException` 处理 RDP 会话降级，`PokeMyself`（自我注入微移动）防屏保，`SendSAS` 解锁远端。这是 MWB 最"重"、最难复刻的部分。

**copywhere**：**明确不做**服务/锁屏/安全桌面（README 已知限制："对 UAC/锁屏等安全桌面无法注入"、"run 需交互式用户会话，不适合作系统服务"）。所有能力在交互式用户桌面内。

**评价**：copywhere 是**有意的范围裁剪**（零依赖单 exe 的产品定位决定的），不是缺陷。但"锁屏后仍能跨屏控制对面"是企业用户的高频需求，MWB 有而 copywhere 无——**列为中长期可选增强，不建议现在做**（引入服务/SCM/UAC 会摧毁"双击即用"的核心卖点）。

---

## 10. 工程结构与代码质量

**MWB**：
- 巨型文件：`SocketStuff.cs` 90KB、`Common.cs` 62KB 是"上帝类"，2008 年单线程 C 代码直译 .NET 的历史包袱重。
- 大量跨线程可变 static（`isDragging/desMachineID/LastX/SwitchLocation…`），`lock` 对象弱标识（`CA2002` 自供）、`Application.DoEvents()` 泵、catch-all 吞异常。
- 死代码多：`RunWithNoAdminRight && false` 硬关单向控制、`frmLogon` 标注 "not used"、两个服务名并存、`Ctrl+Alt+End→旧服务名` 半失效。
- **优点**：`MachinePool.cs` 明确写了"best-effort 语义、无锁内调用、无嵌套锁、写回归测试"的教科书注释；诊断三件套（环形日志 + 反射对象 dump + `GetChecksum` 脱敏）成熟。

**copywhere**：
- 小而清晰：`internal/*` 分层职责单一，接口边界（`Injector`/`Sender`/`Handlers`/`Core`）便于测试替身。
- **71 个单元测试**覆盖 transport/discovery/kvm/trust/pairing；`gofmt`/`go vet`/`go test` 质量门禁。
- 注释密度高且诚实（`RealData` 类的坑、`clip` 线程亲和的根因、混合版本兼容性风险都写进文档）。
- 弱点：`app.go` 单文件 1418 行偏大；无 MWB 式的"运行时对象 dump"自省诊断。

**评价**：copywhere 的工程健康度**显著优于** MWB 的遗留核心；MWB 唯一值得抄的质量实践是**诊断能力**（环形缓冲日志、包收发统计、线程栈转储）——copywhere 目前只有普通 log + 面板日志抽屉。

---

## 11. 逐维度对比总表

| 能力维度 | MWB | copywhere | 胜方 |
|---|---|---|---|
| 部署便利 | 4 进程/服务/runner | 单 exe 双击 | **copywhere** |
| 节点发现 | 手工矩阵 + DNS | UDP 广播自动 | **copywhere** |
| 多网卡/换 IP | 弱（BadIP 记录） | 强（地址合并 + 逐地址重试） | **copywhere** |
| 传输完整性 | 单字节校验和 | sha256 | **copywhere** |
| 传输机密性 | AES-256-CBC（无 AEAD） | 明文 | **MWB**（方向对但实现弱） |
| 准入信任模型 | 持密钥自动成组 | 逐对 TOFU + 指纹绑定 | **copywhere** |
| 细粒度吊销 | 换密钥全组重配 | 单点解除配对 | **copywhere** |
| 剪贴板事件检测 | 事件驱动 | 400ms 轮询 | **MWB** |
| 富文本格式 | RTF/HTML/Text | 纯文本 | **MWB** |
| 文件/目录覆盖 | 单文件、目录拒绝 | 多文件/目录/zip bundle | **copywhere** |
| 系统级文件拖拽 | ✅ 有 | ❌ 无 | **MWB** |
| 锁屏/安全桌面 | ✅ 有 | ❌ 无（有意裁剪） | **MWB** |
| KVM 坐标/DPI | Universal 65535 绝对 | 相对位移 + 手调系数 | **MWB** |
| KVM 多机链式切换 | ✅ NextMachine | ❌ 1对1 | **MWB** |
| KVM 切换稳健性 | 单帧即切，易误触 | push 累积 + 武装切回 | **copywhere** |
| 注入回声区分 | RealData 竞态 hack | LLMHF_INJECTED 正确 | **copywhere** |
| 触控板支持 | 无 | PTP HID 自研识别 | **copywhere** |
| 慢链路容错 | 超时较硬 | 动态预算 + 重试队列 | **copywhere** |
| 图形界面 | WinForms（老） | Web 面板 + SSE + 主题 | **copywhere** |
| 代码可维护性 | 遗留巨石 | 分层 + 71 单测 | **copywhere** |
| 运行时诊断 | 环形日志+对象dump | 普通 log | **MWB** |

---

## 12. 借鉴可能性清单（按性价比排序）

> 每条标注：价值 / 落地成本 / 是否破坏现有协议或产品定位。

### 🟢 强烈推荐（高价值、低成本、不破坏协议）

> ✅ **B1 / B2 / B3 均已落地**（见 CHANGELOG「Unreleased」）：B1 事件驱动检测
> （`internal/clip/watch_windows.go`，轮询保留为兜底）、B3 运行时诊断
> （`internal/diag` + 面板 `/api/debug`）、B2 富格式同步（`type="rich"`，
> 经发现层 `caps:["rich"]` 能力协商向后兼容，见 protocol.md 3.6）。

**B1. 剪贴板检测改为事件驱动（借鉴 MWB `AddClipboardFormatListener`）**
- 现状：400ms 轮询 `GetClipboardSequenceNumber`，有感知延迟且常态唤醒。
- 做法：在 `clip.clipboardThread` 上 `AddClipboardFormatListener(hwnd)` 收 `WM_CLIPBOARDUPDATE` 触发即时读取，保留轮询作降级。因该线程已 `LockOSThread` 且有消息循环基础，改造小。
- 收益：更实时、更省电。**不碰网络协议**。

**B2. 文本保真：RTF / HTML 多格式共存（借鉴 MWB `TEXT_TYPE_SEP` 容器）**
- 现状：只同步 `CF_UNICODETEXT`，从 Word/网页复制丢格式。
- 做法：抄 MWB 的"单容器 + 3 字节类型标签 + GUID 分隔符"打包 TXT/RTF/HTML，或更 Go 风格地在 Header 里加 `formats` 描述 + 分段负载；接收端一次 `SetData` 提交多格式。
- 收益：显著实用性提升。需在 Header 新增可选字段（向后兼容：旧节点只认 text，忽略富格式）。

**B3. 运行时诊断三件套（借鉴 MWB `Logger` 环形缓冲 + 包统计 + 线程栈转储）**
- 做法：KVM/传输收发包计数、活动会话状态、节点表做成内存环形缓冲 + 一个 `/api/debug` 端点 dump；钩子/剪贴板线程加"计数看门狗"，面板日志抽屉旁暴露。
- 收益：copywhere 目前排障靠肉眼 log，跨机问题（尤其 KVM 手感/丢包）难定位。

### 🟡 建议评估（高价值、中等成本）

**B4. KVM 坐标改 Universal 0..65535 绝对注入（借鉴 MWB `ConvertToUniversalValue` + `MOUSEEVENTF.ABSOLUTE`）**
- 现状痛点：README 与 `protocol.md` 都承认"远程光标快慢不一致"无解，退化成**每台手调 `kvm_speed_percent`**，且注明"正解是改绝对坐标注入（未做）"。
- 做法：主控把光标在**其虚拟桌面**的绝对位置归一到 0..65535，副机用 `MOUSEEVENTF.ABSOLUTE` + 自身 `DesktopBounds` 仿射还原。这**直接消除**指针加速/倍率差异（绝对坐标不受本机鼠标速度影响）。
- 代价：需在 `input_windows.go` 加 `MoveAbsNormalized`；入口/切换坐标语义要重设计（现在是 dir+y 比例）。**会改 KVM 消息语义，需版本对齐**。
- 这是**当前 copywhere 最值得做的一项架构升级**——能删掉一整个手调系数及其配套诊断。

**B5. KVM 链式多机切换 `NextMachine` 接力（借鉴 MWB）**
- 现状：A 控制 B 后，B 的屏幕对 A 而言是"副机本机布局自然跨越"，但**不能从 B 再无缝推到 C**（要回 A 重新走边缘）。
- 做法：被控端在收到相对/绝对移动后本地跑边缘检测，命中暴露边缘时回 `{t:"next", target, x, y}` 给主控，由主控切换会话目标。copywhere 已有布局镜像同步，接力只差这一步网络消息。
- 收益：真正 3+ 机器串排时的连贯体验。**新增 KVM 消息类型，向后兼容可加**。

**B6. 大内容"节拍照 + 按需 pull + 反向 push 回退"（借鉴 MWB `Clipboard` beat + `ClipboardAsk`）**
- 现状：copywhere 是"复制即推"（超阈值直接跳过），被动。
- 做法：可选——对超阈值内容只发一个轻量"我有内容"通知（面板角标），用户在面板点"接收"再拉。适合"大文件但只有一两台要"的场景，减少无脑广播。
- 代价：引入反向连接与 pull 语义，中等复杂度。**属于产品模式选择，非纯技术**。

### 🔴 谨慎 / 暂不建议

**B7. 传输加密（借鉴 MWB "每连接派生 AES"的思路，但用现代实现）**
- 分析：MWB 加密方向正确但实现过时（CBC+Zeros 无 AEAD、magic 泄露密钥指纹）。copywhere 目前**明文**，是相对 MWB 的实质安全倒退（尤其 ZeroTier 跨网）。
- 若做：**不要抄 MWB**，直接用 **TLS**（`crypto/tls` 对自签证书 + 配对令牌做 pinning）或 `crypto/nacl` 风格 **X25519 + AES-GCM**，把现有逐对配对令牌当作 PSK/信任根。
- 代价：端口模型（短连接）下每连接握手可接受（文件传输本就长会话）；需重新规划证书/密钥存储。**建议排期，但优先级低于功能正确性**。

**B8. 系统级文件拖拽跨屏（借鉴 MWB `DragDrop` 12 步）**
- 分析：UX 诱人，但实现**极脏且脆弱**（`FindWindow`+定时逼 `DragEnter`、合成 `MouseUp`、完整性边界、竞态），是 MWB 的**反面教材**。且在 KVM 接管期间本机输入被吞，拖拽发起时机难判定。
- 建议：**暂不做**，或仅在 KVM 会话内以"检测到按下-拖动越边"的简化版做，接受单文件。ROI 低。

**B9. 锁屏 / 安全桌面 / Windows 服务（借鉴 MWB 双桌面模型）**
- 分析：企业刚需但**摧毁"零依赖单 exe 双击即用"的核心卖点**（引入 SCM/UAC/SYSTEM 令牌）。
- 建议：**不做**，或作为独立高级发行版（带服务安装器）另议。主工程保持纯用户态。

**B10. 定长二进制协议 / 4 机矩阵 / 常连接网状**
- **全部不建议**：copywhere 的 JSON + 短连接 + 广播拓扑在目标场景更优，MWB 这些是性能与时代约束下的取舍，照搬只会增加复杂度、破坏可调试性与产品定位。

### 明确"不要学"的 MWB 反面清单
1. 钩子回调线程内**同步网络 IO + `Thread.Sleep(10)`** → copywhere 已用"回调零阻塞 + 合拍协程"正确规避，保持。
2. `RealData` 竞态式注入回声判定 → copywhere 已用 `LLMHF_INJECTED`，保持。
3. 全局可变 static 状态 + `Application.DoEvents()` + catch-all → copywhere 的 goroutine + channel + 显式错误保持。
4. CBC + `PaddingMode.Zeros` 无 AEAD、magic 泄露密钥指纹 → 若做加密务必用 AEAD。
5. 遗留死代码（`&& false` 分支、双服务名）→ 保持 copywhere 的死代码清理纪律。

---

## 13. 结论

1. **两个项目解决的是同一问题的两端**：MWB 为"4 台固定机 + 低延迟软 KVM"深度优化（常连接、事件驱动、Universal 坐标、锁屏），copywhere 为"任意节点 + 零部署 + 内容同步"深度优化（广播发现、sha256、TOFU 配对、Web 面板、多文件/目录）。**copywhere 在部署、发现、可靠性、准入信任、文件覆盖、KVM 稳健性、工程质量上整体领先；MWB 在传输机密性、剪贴板事件实时性、富格式、系统级拖拽、锁屏、KVM 绝对坐标与链式多机上领先。**

2. **最值得立即借鉴的三点**：
   - **B4 Universal 65535 绝对坐标注入** —— 直接解决 copywhere 自认无解的"跨机光标快慢不一致"，可删掉整套手调系数。
   - **B1 事件驱动剪贴板检测** + **B2 RTF/HTML 富格式** —— 低成本实用提升。
   - **B3 运行时诊断** —— 补齐跨机排障能力。

3. **应借鉴思路但要重写的**：传输加密（用 TLS/AEAD，别抄 CBC 老实现）、链式多机切换（`NextMachine` 语义可抄，载体用 JSON 新消息）、大内容按需 pull（产品模式）。

4. **坚决不抄的**：多进程/服务/Global Mutex 复杂度、定长二进制协议、手工机器矩阵、钩子内同步 IO、`RealData` 竞态、系统级拖拽的脏状态机、锁屏双桌面模型（除非另立高级发行版）。

> 一句话：**copywhere 的骨架比 MWB 更现代，不该向 MWB 的架构靠拢；应定向摘取 MWB 在"低延迟输入体验"上沉淀了 15 年的三个具体技术点（绝对坐标、链式切换、事件驱动），其余保持自有路线。**

---
*报告生成于对两代码库的静态源码精读 + 三份 MWB 子系统独立分析交叉验证。MWB 侧结论以 `MouseWithoutBorders/App` 下类/函数/常量的具体实现为据；copywhere 侧以各 `internal/*` 包与 `docs/protocol.md` 为据。*
