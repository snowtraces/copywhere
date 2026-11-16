# 构建 Microsoft Store 用的 MSIX 包（本地可侧载、可跑 WACK）。
# 用法与提交前注意事项见 docs/release.md「构建 MSIX 商店包」。
param(
    [string]$Version = "",
    [string]$Publisher = "CN=copywhere-dev",
    [string]$IdentityName = "copywhere",
    [string]$PublisherDisplayName = "copywhere",
    [string]$Exe = "",
    [string]$OutDir = "",
    [string]$SdkBin = "",
    [switch]$NoSign
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$root = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
$tools = Join-Path $root 'tools\msix'
if ($OutDir -eq '') { $OutDir = Join-Path $root 'dist\msix' }
$OutDir = [System.IO.Path]::GetFullPath($OutDir)

# 版本号缺省取 versioninfo.json，MSIX 必须是四段式。
if ($Version -eq '') {
    $vi = Get-Content (Join-Path $root 'cmd\copywhere\versioninfo.json') -Raw -Encoding UTF8 | ConvertFrom-Json
    $Version = $vi.StringFileInfo.FileVersion
}
if ($Version -notmatch '^\d+\.\d+\.\d+\.\d+$') {
    throw "版本号必须是四段式，例如 0.6.4.0（当前: $Version）"
}

# 定位 Windows SDK 的 makeappx / signtool（自动选最高版本）。
if ($SdkBin -eq '') {
    $kitRoot = Join-Path ([Environment]::GetFolderPath('ProgramFilesX86')) 'Windows Kits\10\bin'
    $cand = Get-ChildItem -Path $kitRoot -Directory -ErrorAction SilentlyContinue |
            Where-Object { Test-Path (Join-Path $_.FullName 'x64\makeappx.exe') } |
            Sort-Object Name -Descending | Select-Object -First 1
    if ($cand) { $SdkBin = Join-Path $cand.FullName 'x64' }
}
$makeappx = if ($SdkBin -ne '') { Join-Path $SdkBin 'makeappx.exe' } else { '' }
if (-not (Test-Path $makeappx)) {
    throw "找不到 makeappx.exe，请安装 Windows SDK 或用 -SdkBin 指定 bin\x64 目录"
}
$signtool = Join-Path $SdkBin 'signtool.exe'

# 干净的 staging 目录。
$stage = Join-Path $OutDir 'staging'
if (Test-Path $stage) { Remove-Item $stage -Recurse -Force }
New-Item -ItemType Directory -Force -Path (Join-Path $stage 'Assets') | Out-Null
$exePath = Join-Path $stage 'copywhere.exe'

if ($Exe -ne '') {
    if (-not (Test-Path $Exe)) { throw "找不到 exe: $Exe" }
    Copy-Item $Exe $exePath -Force
    Write-Host "使用已有 exe: $Exe（注意它可能不是 store flavor 构建）"
} else {
    # 商店版注入 storeBuild=1：首次生成的默认配置关闭 KVM 与右键菜单。
    # main.buildFlavor 仍保持 gui，避免改变 GUI 子系统 / 命令行委派行为。
    $ver3 = ($Version -split '\.')[0..2] -join '.'
    $ldflags = "-s -w -H windowsgui -X copywhere/internal/app.Version=v$ver3 -X main.buildFlavor=gui -X copywhere/internal/config.storeBuild=1"
    Write-Host "构建 store 版 exe (v$ver3) ..."
    Push-Location $root
    try {
        & go build -trimpath -ldflags $ldflags -o $exePath ./cmd/copywhere
        if ($LASTEXITCODE -ne 0) { throw "go build 失败 (exit $LASTEXITCODE)" }
    } finally { Pop-Location }
}

Copy-Item (Join-Path $tools 'Assets\*.png') (Join-Path $stage 'Assets') -Force

# 替换清单占位符；AppxManifest.xml 必须写成 UTF-8 无 BOM。
$tpl = Get-Content (Join-Path $tools 'AppxManifest.xml') -Raw -Encoding UTF8
$rep = @{
    '{{IDENTITY_NAME}}'          = $IdentityName
    '{{PUBLISHER}}'              = $Publisher
    '{{VERSION}}'                = $Version
    '{{PUBLISHER_DISPLAY_NAME}}' = $PublisherDisplayName
}
foreach ($k in $rep.Keys) { $tpl = $tpl.Replace($k, $rep[$k]) }
$utf8NoBom = New-Object System.Text.UTF8Encoding($false)
[System.IO.File]::WriteAllText((Join-Path $stage 'AppxManifest.xml'), $tpl, $utf8NoBom)

$pkg = Join-Path $OutDir ("copywhere_{0}_x64.msix" -f $Version)
Write-Host "打包 $pkg"
& $makeappx pack /d $stage /p $pkg /o
if ($LASTEXITCODE -ne 0) { throw "makeappx pack 失败 (exit $LASTEXITCODE)" }

$cerPath = Join-Path $OutDir 'copywhere-dev.cer'
if (-not $NoSign) {
    if (-not (Test-Path $signtool)) { throw "找不到 signtool.exe: $signtool" }
    # 商店会用微软证书重签，这里的自签证书只用于本地侧载 / WACK / 上传校验。
    # 证书 Subject 必须与清单 Identity/Publisher 完全一致。
    $cert = Get-ChildItem 'Cert:\CurrentUser\My' -ErrorAction SilentlyContinue |
            Where-Object { $_.Subject -eq $Publisher -and $_.HasPrivateKey } |
            Select-Object -First 1
    if (-not $cert) {
        Write-Host "创建自签开发证书 $Publisher ..."
        $cert = New-SelfSignedCertificate -Type CodeSigningCert -Subject $Publisher `
                  -KeyUsage DigitalSignature -FriendlyName 'copywhere MSIX dev cert' `
                  -CertStoreLocation 'Cert:\CurrentUser\My' -NotAfter (Get-Date).AddYears(3)
    }
    Export-Certificate -Cert $cert -FilePath $cerPath -Type CERT -Force | Out-Null
    & $signtool sign /fd SHA256 /sha1 $cert.Thumbprint $pkg
    if ($LASTEXITCODE -ne 0) { throw "signtool 签名失败 (exit $LASTEXITCODE)" }
}

Write-Host ''
Write-Host "MSIX 已生成: $pkg"
Write-Host ("  大小: {0:N1} MB" -f ((Get-Item $pkg).Length / 1MB))
Write-Host "  Identity Name : $IdentityName"
Write-Host "  Publisher     : $Publisher"
Write-Host "  Version       : $Version"
Write-Host ''
if (-not $NoSign) {
    Write-Host '本地侧载（先信任开发证书）：'
    Write-Host "  certutil -addstore -user TrustedPeople `"$cerPath`""
    Write-Host "  Add-AppxPackage -Path `"$pkg`""
    Write-Host ''
}
Write-Host '提交 Partner Center 前：'
Write-Host '  1) 用 Product identity 页的值覆盖 -IdentityName / -Publisher / -PublisherDisplayName'
Write-Host '  2) 跑 Windows App Certification Kit (WACK)，再上传 .msix'
Write-Host '  3) Submission options 里说明 runFullTrust 的理由，并披露 KVM 全局钩子行为'
