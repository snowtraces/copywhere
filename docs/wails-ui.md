# Wails v3 桌面窗口（实验性）

> 基线：`78a5626`（v0.4.0）。本分支（`feat/wails-ui`）在此基线上新增第三套前端：
> 浏览器面板（`gui`）与终端界面（`run`）之外的 **Wails v3 独立窗口**。
> 状态：可运行、可构建、有单测；尚未并入发布流水线与默认入口。

## 1. 为什么做、做了什么

网页面板的痛点不在功能，而在「承载」：浏览器标签页身份错位、Alt+Tab 混在网页里、
关掉标签页服务还在跑、二次双击只多开一个标签页。Wails v3 提供的是 L1 级
「独立应用窗口 + WebView2 + 原生托盘/菜单/对话框」，正好覆盖这些痛点，
同时保留 Web 前端在富交互（时间线、布局画布、灯箱）上的成本优势。

本分支因此**不重写核心**，只新增一层桌面壳与新前端：

- 完整复用 `internal/app`（发现/传输/剪贴板/KVM）、`internal/config`、
  `internal/webui`（事件总线与配置应用）；
- 新增 `internal/wailsui`：Wails 应用装配、绑定服务、事件桥、托盘、窗口生命周期；
- 新增 `internal/wailsui/frontend`：不依赖 Node/npm 的静态 HTML/CSS/JS 前端，
  沿用网页面板的 WIREFRAME / TERMINAL 视觉（等宽字体、1px 线框、终端配色、深浅双主题）。

## 2. 目录结构与职责

```
cmd/copywhere/
  main.go              # 新增 wails 子命令；exe 名含 wails 时无参数默认进入窗口模式
  wails.go             # 单实例、隔离配置、日志落盘、本地 HTTP 面板回退、启动/收尾
internal/wailsui/
  app.go               # Wails 应用装配：窗口、托盘、事件桥、拖放、尺寸记忆、退出
  service.go           # 绑定给前端的 Service（视图 + 操作），Core 接口便于测试
  util.go              # 图片预览白名单 / base64
  activate_windows.go  # 二次启动：按窗口类+标题+同 exe 名查找并唤回
  frontend/            # index.html + app.css + app.js（go:embed）
internal/webui/
  config_apply.go      # 从 apiSetConfig 抽出的 ApplyConfig（面板与桌面端共用）
  bus.go               # FileView 改为导出，供桌面端复用记录视图
  icon_export.go       # AppIcon/FaviconPNG 导出，避免二进制资源重复入库
```

## 3. 通信模型

- **前端 → Go**：`Call.ByName("copywhere/internal/wailsui.Service.<Method>", ...)`
  （`Service` 的 FQN 由包路径 + 类型名生成，`service_test.go` 锁死该值与关键方法）。
- **Go → 前端**：
  - `cw:event`：与 `webui.Bus` 同构的 `{type, data}`。`log` / `file` / `progress` /
    `pair_request` 直接转发；`peers` / `state` 由桌面端每 2 秒用自己的 camelCase
    视图推送（旧面板的 snake_case 快照被过滤，避免两套字段混用）。
  - `cw:toast`：轻提示（拖放投送结果、操作失败原因）。
- **拖放**：窗口开启 `EnableFileDrop`，Go 端监听 `WindowFilesDropped` 拿到真实路径，
  直接 `core.SendFiles(paths, 0)`；前端只负责高亮与提示。
- **配置**：`SaveSettings` 调用 `webui.ApplyConfig`，与网页面板共享校验、落盘与
  热更新（KVM 启停、触控板手势、邻居同步）。

## 4. 生命周期

| 事项 | 行为 |
| --- | --- |
| 关闭按钮 | `WindowClosing` 钩子里 `Hide()` + `Cancel()`，进程与同步继续，托盘常驻 |
| 托盘 | 打开窗口 / 自动同步复选框 / 打开接收目录 / 退出；左键打开菜单 |
| 退出 | 标记 quitting → 保存窗口尺寸 → 取消核心上下文 → `app.Quit()`；宿主随后 `Wait()` |
| 二次启动 | 命名互斥锁拦截 → 枚举同 exe 名的 Wails 主窗口并 `SW_RESTORE` + 置前（约 200ms） |
| 尺寸记忆 | 拖动/缩放 400ms 去抖落盘 `~/.copywhere/wails-window.json`，启动恢复尺寸、位置居中 |
| 单实例 | 与 `gui` / `run` 共用 `Local\copywhere-singleton`；窗口模式仍有本地 HTTP 面板作为回退 |

## 5. 前端功能

- 工作台：今天/昨天/日期分组；左收右发气泡；文本展开/复制；文件类型徽标卡片；
  接收目录图片缩略图 + 灯箱；失败状态着色；实时进度条；清空显示（不影响对端）。
- 设备坞：本机卡片（阈值/已配对/监听方式）、在线节点（地址/活跃/配对/鉴权拒绝/
  左右邻标签）、配对/解除/设为左邻右邻。
- 屏幕布局：左邻 / 本机 / 右邻三屏画布，拖拽或点选配置，清除按钮；跨屏总开关。
- 设置：节点名、接收目录、阈值、自动回贴、文本同步、跨屏开关、触控板手势与倍率、
  移动合拍/重排节拍/速度、已配对设备管理、主题、关于（版本/配置/端口/监听方式）。
- 日志抽屉：`Ctrl+L` 开关、错误行着色、未读角标、复制、清空（仅本窗口）。
- 快捷键：`Ctrl+K` 聚焦输入、`Ctrl+,` 设置、`Enter` 发送、`Shift+Enter` 换行、
  `Esc` 关闭弹层（配对确认不响应 Esc，须明确裁决）。

## 6. 界面预览

工作台（对话式时间线、设备坞、实时进度、日志角标）：

![Wails 工作台](images/wails-workspace.png)

屏幕布局（左邻 / 本机 / 右邻，拖拽或点选配置，即时同步对端）：

![Wails 屏幕布局](images/wails-layout.png)

设置（与网页面板共用 ApplyConfig，保存即生效）：

![Wails 设置](images/wails-settings.png)

> 截图由 CDP（`CW_WAILS_DEBUG_PORT`）对真实窗口截取，其中节点名/IP/记录为演示数据。

## 7. 构建与运行

要求：Go 1.26+（与 go.mod 一致）、Windows 10 1803+ / Windows 11、系统 WebView2 运行时。
Wails 依赖固定为 `github.com/wailsapp/wails/v3 v3.1.0`，Windows 构建不需要 CGO。
发布构建实测 15,130,112 B（v0.4.0 基线 9,192,448 B，+5.9 MB：Wails 运行时与
WebView2 宿主代码，不含系统 WebView2 本身）。

```powershell
# 开发构建（控制台可见，便于看日志）
go build -o copywhere-wails.exe ./cmd/copywhere
copywhere-wails.exe                 # exe 名含 wails → 无参数进入窗口模式
copywhere-wails.exe wails           # 显式子命令

# 发布构建
go build -trimpath -ldflags "-s -w -H windowsgui -X copywhere/internal/app.Version=v0.4.0-wails" `
  -o copywhere-wails.exe ./cmd/copywhere
```

冒烟测试（自动退出，隔离端口，可与本机正在运行的实例并存）：

```powershell
copywhere-wails.exe wails -config .\smoke-config.json -smoke 8s
# 退出时会把事件总线日志打到控制台：[smoke] ...
```

调试：设置 `CW_WAILS_DEBUG_PORT=9222` 后可用 CDP 连接 WebView2
（`http://127.0.0.1:9222/json/list`）截图或排查前端；`CW_WAILS_DEVTOOLS=1` 启动即打开
开发者工具。

运行时日志写入 `~/.copywhere/wails.log`（超过 4MB 滚动为 `.1`）：服务启动后
`app.Options.LogFile` 让日志同时进入事件总线与磁盘，窗口崩溃/闪退也有据可查。

## 8. 验证记录（本机实测）

- `go build ./...`、`go vet ./internal/wailsui ./internal/webui ./cmd/copywhere` 通过。
- `go test ./internal/wailsui` 覆盖：状态/节点视图、发送校验、设置保存与落盘、
  KVM 邻居、配对流程、图片预览安全边界、事件桥、绑定 FQN。
- 冒烟日志确认链路：`[WebView2] Environment created successfully` →
  `Wails 桌面窗口已启动：1120x760` → `Wails 前端已加载：<UA>` → 自动退出 `err=<nil>`。
- CDP 实测：设置保存后 Go 端 `State().nodeName` 同步变化并落盘；
  `SetKVMNeighbor` 后 `State().kvmLeft` 生效；无节点发送文本返回可读错误。
- Win32 实测：`WM_CLOSE` 后窗口隐藏、进程存活；二次启动 193ms 内恢复原窗口并退出 0。

## 9. 已知限制与后续

1. **仅 Windows**：核心服务本身就是 Windows 专用；Wails 窗口未做 Linux/macOS 适配。
2. **依赖 WebView2 运行时**：企业精简镜像/LTSC 需预装；缺失时窗口无法创建。
3. **绑定 FQN 硬编码**：前端使用 `Call.ByName`，避免引入 `wails3` CLI 生成绑定；
   若模块名或包路径变化，需同步 `app.js` 中的常量（有单测守护）。
4. **无前端工程化**：没有 Node 构建、HMR、打包压缩；换来的是 `go build` 单命令。
5. **本地 HTTP 面板仍在**：窗口模式也会监听 `127.0.0.1`（随机令牌），
   作为排障与「已有实例」回退入口；若要求纯进程内、无本地端口，可在
   `cmd/copywhere/wails.go` 中去掉 `webui.NewServer`（同时失去面板回退与右键发送定位）。
6. **二次唤回按 exe 名匹配**：`copywhere-wails.exe` 改名为其它名字后，第二次启动
   只会提示「已有实例」而不会唤回窗口。
7. **未接入发布**：`.github/workflows/build.yml`、`docs/release.md` 未包含 Wails 产物；
   `wails3` CLI 的 manifest/图标/绑定生成流程也未接入。
8. **与网页面板的前端未统一**：两套前端共享视觉与 Go 内核，但 HTML/JS 各自维护；
   后续若要收敛，可把 `webui/static/index.html` 拆成 tokens + 组件 + 传输适配层，
   让两侧共用同一份 UI 代码（传输方式由适配层注入）。
