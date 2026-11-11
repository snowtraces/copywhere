# 版本发布流程

本文描述 copywhere 的标准发版步骤。前置条件：Go 1.26.4+、goversioninfo、git 推送权限、
gh CLI（仅创建 GitHub Release 需要；也可用 `GH_TOKEN` 环境变量替代登录态）。

## 1. 整理版本内容

- 确认所有待发布改动已合入 `main` 并推送。
- 更新 `CHANGELOG.md`：把"未发布"小节改为 `## vX.Y.Z (YYYY-MM-DD)`，整理条目
  （新增 / 修复 / 其他）；若无破坏性变更，次版本号 +1（`v0.2.0 → v0.2.1`），
  有新功能模块可升次版本号（如 KVM 加入即 `v0.1 → v0.2`）。
- 同步检查 `README.md`、`docs/protocol.md` 中的阈值、端口、配置项、协议字段
  是否与代码一致。

## 2. 质量门禁

```powershell
go build ./...                 # 编译
go test ./...                  # 全部单元测试
go vet -unsafeptr=false ./...  # 静态检查（unsafeptr 关闭原因见 internal/clip 包注释）
gofmt -l .                     # 格式检查，输出应为空
```

## 3. 构建产物

先把 `cmd/copywhere/versioninfo.json` 中的 FileVersion 和 ProductVersion 同步到发布版本，
再从仓库根目录重新生成 Windows 版本资源：

```powershell
goversioninfo -o cmd/copywhere/rsrc_windows_amd64.syso cmd/copywhere/versioninfo.json
```

这一步确保 exe 属性页中的文件版本与产品版本匹配发布版本；只设置 Go 的 `-ldflags` 版本
不会更新 Windows 资源属性。

```powershell
$v = "vX.Y.Z"   # 与 CHANGELOG / git tag 保持一致
go build -trimpath -ldflags "-s -w -X copywhere/internal/app.Version=$v" `
  -o copywhere-$v-windows-amd64.exe ./cmd/copywhere
.\copywhere-$v-windows-amd64.exe version   # 应输出 copywhere vX.Y.Z
```

**不要加 `-H windowsgui`**：GUI 子系统会让 PowerShell 不等待进程，
`copywhere nodes`、`copywhere version` 等子命令表现为"没输出、命令立刻返回"，
`|` 与 `>` 也拿不到内容（cmd.exe 会等待，但 PowerShell 不会）。
控制台子系统下，双击启动时 `hideOwnConsole()` 会立即隐藏系统新建的控制台窗口，
托盘 + WebView2 窗口照常工作（开发构建一直如此）。发布前请在 PowerShell 里实测
`.\copywhere-$v-windows-amd64.exe version` 与 `.\copywhere-$v-windows-amd64.exe nodes`：
必须等命令执行完、有输出、`$LASTEXITCODE` 为 0。

exe 图标与版本信息由 `cmd/copywhere/rsrc_windows_amd64.syso` 自动嵌入，无需额外步骤。

产物运行需要系统 WebView2 运行时（Win10 1803+ / Win11 一般自带）；发布说明中应保留该前提。

产物命名规范：`copywhere-$v-windows-amd64.exe`（未来如有跨平台再加后缀）。
产物只进 GitHub Release 附件，**不要提交进 git 仓库**。

## 4. 提交与打标签

```powershell
git add -A
git commit -m "release: vX.Y.Z"
git tag -a vX.Y.Z -m "copywhere vX.Y.Z"
git push origin main --follow-tags
```

## 5. 创建 GitHub Release

```powershell
gh release create vX.Y.Z .\copywhere-vX.Y.Z-windows-amd64.exe `
  --title "copywhere vX.Y.Z" --notes-file release-notes.md
```

- `release-notes.md` 内容取自 CHANGELOG 对应版本小节（面向用户的措辞）。
- gh 未登录时：`gh auth login --with-token` 需要 `read:org` scope；若用
  Git 凭据管理器里已有的 OAuth token（缺该 scope），改为在单条命令前设置
  `$env:GH_TOKEN = <token>`，不要尝试存储登录态。

## 6. 发布校验

```powershell
gh release view vX.Y.Z    # 确认 tag、说明、资产齐全
```

下载附件运行 `copywhere version`，应输出 `vX.Y.Z`。最后把新 exe 部署到
各节点替换旧版（KVM/传输协议有变更时，**所有节点必须同步升级**）。

## 快速检查清单

- [ ] CHANGELOG 已定稿（版本号 + 日期）
- [ ] build / test / vet / gofmt 全部通过
- [ ] 产物 version 输出正确
- [ ] tag 已推送（`git ls-remote --tags origin` 可见）
- [ ] Release 附件可下载、说明完整
- [ ] 各节点已部署新版本
