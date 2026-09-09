# copywhere

局域网剪贴板/文件自动同步工具（Windows，Go 实现）。

在一台机器上 **Ctrl+C** 复制小于阈值的文件（默认 10MB）或文本，局域网内所有在线
`copywhere` 节点会自动收到：文件落盘到接收目录并写回其剪贴板，对端直接 **Ctrl+V** 即可。

## 特性

- **节点自动发现**：UDP 广播（各网段定向广播 + 回环），无需配置 IP，节点上下线自动感知
- **多网卡地址合并**：同一节点从多个网络（局域网 + ZeroTier 等）广播只算一个节点；
  全部地址保留并展示（首选与本机同网段的直连路径，其余以 `└─ 备选地址` 逐行列出），
  发送时逐地址尝试，任一可达即成功
- **剪贴板模式**：监控本机剪贴板，文件（CF_HDROP）与文本自动同步；接收端自动回贴
- **三页终端界面**：`run` 默认显示 1 日志 / 2 在线节点 / 3 文件记录 三个标签页，
  可上下翻页浏览历史；`-plain` 或输出非终端时退化为纯日志
- **阈值控制**：小于阈值的剪贴板内容自动同步（默认 10MB，`max_auto_copy_mb` 可配，0 为不限）
- **多文件/文件夹**：自动打包成 zip 传输，对端收到 zip 并回贴
- **手动发送**：`copywhere send` 不受阈值限制，可发大文件（单文件上限 4GB）
- **可靠传输**：TCP + sha256 校验 + 临时文件原子改名；同名同内容自动去重
- **慢链路友好**：超时预算按 256KB/s 估算（ZeroTier 中继等慢链路不误杀），
  传输失败时对端日志会记录具体原因
- **token 不一致不重试**：对端拒绝（token 不同）后提示一次即暂停向该节点发送，
  不会高频重试；对端修正 token 并重启后自动恢复
- **鼠标键盘跨屏（实验性）**：光标推向屏幕边缘即可控制相邻机器，键盘/滚轮同步；
  复用同一套发现与鉴权，`Ctrl+Alt+Shift+X` 紧急退出
- **防回环**：剪贴板序号 + 接收冷却期双重防护，不会自己同步自己
- **临时不可达重试**：对端短暂离线时，内容暂存 60 秒自动重试
- **鉴权**：所有节点需配置相同 token，token 不一致无法传输

## 编译

要求 Go 1.21+（Windows 下开发与使用）。

```powershell
# 开发构建
go build -o copywhere.exe ./cmd/copywhere

# 发布构建（注入版本号；$v 与 CHANGELOG / git tag 保持一致）
$v = "v0.2.0"
go build -trimpath -ldflags "-s -w -X copywhere/internal/app.Version=$v" `
  -o copywhere.exe ./cmd/copywhere

go test ./...        # 运行全部单元测试
copywhere version    # 查看版本号
```

协议与设计细节见 [docs/protocol.md](docs/protocol.md)；版本发布流程见
[docs/release.md](docs/release.md)。

## 快速开始（两台机器 A、B）

1. 在两台机器上各自生成默认配置（首次运行 `run` 也会自动生成）：

   ```powershell
   .\copywhere.exe init
   ```

2. **让两台机器 token 一致**：打开 `~/.copywhere/config.json`，把 A 的
   `token` 复制到 B（或两边改成同一个值）。建议顺便改一个友好的 `node_name`。

3. 防火墙提示：发现/传输/KVM 三个端口（默认 47830~47832）需要允许入站。
   首次运行时 Windows 会弹窗询问，选择"允许"（至少专用网络）；按需也可用
   `netsh advfirewall` 按程序放行。

4. 两边启动服务：

   ```powershell
   .\copywhere.exe run
   ```

5. 验证发现：

   ```powershell
   .\copywhere.exe nodes
   ```

6. 使用：在 A 上资源管理器里复制一个小文件 → B 的日志显示"已接收文件 …"，在 B
   上直接 **Ctrl+V** 即可粘贴该文件。复制文本同理。

## 命令

| 命令 | 说明 |
| --- | --- |
| `copywhere init` | 生成默认配置（含随机 token） |
| `copywhere run [-config 路径] [-plain]` | 启动常驻服务；默认三页 TUI，`-plain` 纯日志 |
| `copywhere nodes [-config 路径] [-wait 4s]` | 扫描并列出在线节点（含多网卡地址合并结果） |
| `copywhere send [-config 路径] [-text 内容] [-wait 4s] 文件...` | 立即发送到所有在线节点，不受阈值限制 |
| `copywhere clip-test` | 本机剪贴板读写自检（覆盖并尝试恢复剪贴板） |

### TUI 快捷键（`run` 界面）

| 按键 | 功能 |
| --- | --- |
| `1` / `2` / `3`、`Tab`、`←` `→` | 切换 日志 / 在线节点 / 文件记录 |
| `↑` `↓`、`PgUp` `PgDn` | 滚动日志与文件记录（历史最多保留 1000 行） |
| `q` / `Esc` / `Ctrl+C` | 退出 |

节点页显示格式：`[本机]名称(ip)` 与 `[远程]名称(ip)`，远程节点的每个已知地址
（备选地址）单独一行列出。

## 配置（`~/.copywhere/config.json`）

| 字段 | 默认 | 说明 |
| --- | --- | --- |
| `node_name` | 主机名 | 节点显示名 |
| `token` | 随机 | 传输鉴权令牌，**所有节点必须一致** |
| `discovery_port` | 47830 | UDP 发现端口 |
| `transfer_port` | 47831 | TCP 传输端口 |
| `max_auto_copy_mb` | 10 | 剪贴板自动同步阈值（MB），0 = 不限制 |
| `receive_dir` | `~/.copywhere/files` | 接收文件保存目录 |
| `auto_paste` | true | 收到文件后自动写入本机剪贴板 |
| `text_sync` | true | 同步剪贴板文本 |
| `announce_interval_sec` | 2 | 广播间隔 |
| `peer_ttl_sec` | 12 | 节点存活判定时长（约为广播间隔的 5~6 倍） |
| `kvm_enabled` | true | 鼠标键盘跨屏开关 |
| `kvm_port` | 47832 | KVM 监听端口 |
| `kvm_left` | 空 | 左边缘邻居的**节点名**（`node_name`，与对端配置互为镜像） |
| `kvm_right` | 空 | 右边缘邻居的节点名 |
| `kvm_entry_monitor` | -1 | 被控入口显示器下标（`-1` = 恒用主显示器） |

## 鼠标键盘跨屏（KVM，实验性）

在两台互为邻居的机器上分别配置（A 在左、B 在右）：

```jsonc
// A（左边机器）
{ "kvm_right": "B的node_name" }
// B（右边机器）
{ "kvm_left": "A的node_name" }
```

重启 `run` 后即可使用：

- 在 A 上把光标推向**右边缘并继续推**（累计约 16px）→ 控制权切到 B，键盘/滚轮同步转发
- 在 B 上把光标推回**左边缘并继续推** → 控制权交还 A
- 紧急退出：`Ctrl+Alt+Shift+X`
- 主控机失联 5 秒，副机自动释放控制

多显示器：被控入口固定在其**主显示器**的共享边缘，入口高度跟随主控光标比例
（主控在自身屏 50% 高度处推出 → 落在被控主屏 50% 高度处），双屏、不同缩放率
下也不会跑到屏幕角落；副机本地多屏由操作系统自然跨越，切换/切回只在其
"暴露边缘"（外侧）触发。

当前限制（MVP）：对 UAC/锁屏等安全桌面无法注入输入；按键按 VK 码转发，中文输入
依赖对端输入法（跨机传文字建议直接用剪贴板同步）；跨 ZeroTier 中继时延迟取决于
链路质量。协议细节见 [docs/protocol.md](docs/protocol.md)。

## 节点指纹与多网卡地址

- 节点 ID 由 Windows MachineGuid 派生的**稳定指纹**标识：同一台机器重启后 ID 不变，
  对端不会把重启后的机器当成新节点。
- 一个节点从多个网络（局域网、ZeroTier 等）广播来的**所有地址全部保留并展示**，
  不做提前去重；仅做优选排序（与本机同网段的直连地址排最前）。
- 发送时**依次尝试节点的每个地址，任一可达即成功**，优选路径失效不会导致失联；
  超过 5 分钟无公告的地址才会从候选中剔除。

## 已知限制 / 注意事项

- 仅支持 Windows（剪贴板使用 Win32 API）；协议与发现层是跨平台的。
- 自动发现依赖 UDP 广播：跨路由/跨 VLAN 或开启了 AP 隔离的网段无法互相发现
  （可手动 `send`，或后续扩展支持指定对端 IP）。
- `run` 需要在交互式用户会话中运行（写剪贴板的要求），不适合作为系统服务。
- 接收端自动回贴的是"单个文件"或"zip 包"；多文件会以 zip 形式出现在剪贴板。
- 超过阈值的剪贴板复制不会自动同步（日志会提示），需要时用 `send` 手动发。

## 目录结构

```
cmd/copywhere/        CLI 入口
internal/app/         组装、发送/重试、zip 打包
internal/clip/        Windows 剪贴板读写（CF_HDROP / CF_UNICODETEXT）
internal/config/      配置
internal/discovery/   UDP 广播发现与节点表（指纹、会话、多 IP 合并与优选）
internal/monitor/     剪贴板轮询监控与防回环
internal/transport/   TCP 传输协议（服务端/客户端）
internal/kvm/         鼠标键盘跨屏（软 KVM）
internal/input/       全局输入捕获（钩子+Raw Input）与 SendInput 注入
internal/ui/          三页终端界面（日志/在线节点/文件记录）
internal/bytesize/    字节数格式化
docs/protocol.md      协议与设计说明
docs/release.md       版本发布流程
```

## 许可证

[MIT](LICENSE)

