# 生成 copywhere 应用图标（多尺寸 ICO + 预览 PNG）。
# 用法: pwsh -File tools/icon/gen_icon.ps1
# 设计: 折纸光翼 (Origami Dart / 投送飞梭)
#       深空冷光渐变底板 + 陶瓷纯白与电光青折纸光翼，朝向东北 45° 破空飞驰。
#       极简几何线条，极高辨识度与穿透力，在 16x16 / 24x24 / 32x32 托盘下锐利醒目。
# 注意: 主体应用图标保持本折纸设计不变（Web favicon 与此同主题）。
#       托盘菜单小图标另见 gen_menu_icons.ps1（浅色线框风格）。
param(
    [string]$OutIco = "$PSScriptRoot\..\..\internal\webui\icon.ico",
    [string]$OutSvg = "$PSScriptRoot\..\..\internal\webui\favicon.svg",
    [string]$OutPng = "$PSScriptRoot\..\..\internal\webui\favicon.png",
    [string]$OutPreview = "$PSScriptRoot\preview.png"
)
Set-StrictMode -Version Latest
Add-Type -AssemblyName System.Drawing

# ---------- 绘制主图（1024 超采样） ----------
$S = 1024
$bmp = New-Object System.Drawing.Bitmap($S, $S)
$g = [System.Drawing.Graphics]::FromImage($bmp)
$g.SmoothingMode = [System.Drawing.Drawing2D.SmoothingMode]::AntiAlias
$g.PixelOffsetMode = [System.Drawing.Drawing2D.PixelOffsetMode]::HighQuality

function New-RoundedRect([float]$x, [float]$y, [float]$w, [float]$h, [float]$r) {
    $p = New-Object System.Drawing.Drawing2D.GraphicsPath
    $d = $r * 2
    $p.AddArc($x, $y, $d, $d, 180, 90)
    $p.AddArc(($x + $w - $d), $y, $d, $d, 270, 90)
    $p.AddArc(($x + $w - $d), ($y + $h - $d), $d, $d, 0, 90)
    $p.AddArc($x, ($y + $h - $d), $d, $d, 90, 90)
    $p.CloseFigure()
    return $p
}

# 1. 底板：对角冷光渐变圆角方形（深空科技蓝 -> 电光青蓝）
$m = 48.0; $r = 220.0
$tile = New-RoundedRect $m $m ($S - 2*$m) ($S - 2*$m) $r
$p1 = New-Object System.Drawing.PointF(80.0, 50.0)
$p2 = New-Object System.Drawing.PointF(944.0, 974.0)
$c1 = [System.Drawing.Color]::FromArgb(255, 0x1E, 0x48, 0xFA) # Royal Blue
$c2 = [System.Drawing.Color]::FromArgb(255, 0x02, 0x84, 0xC7) # Electric Sky Blue
$baseGrad = New-Object System.Drawing.Drawing2D.LinearGradientBrush($p1, $p2, $c1, $c2)
$g.FillPath($baseGrad, $tile)

# 2. 内边缘微光描边（Fluent 风格高质感光效）
$innerRim = New-RoundedRect ($m + 4) ($m + 4) ($S - 2*$m - 8) ($S - 2*$m - 8) ($r - 2)
$rimPen = New-Object System.Drawing.Pen([System.Drawing.Color]::FromArgb(60, 255, 255, 255), 5)
$g.DrawPath($rimPen, $innerRim)

# 3. 折纸光翼几何坐标（东北 45°）
$tip = New-Object System.Drawing.PointF(764, 260)
$leftWing = New-Object System.Drawing.PointF(240, 484)
$rightWing = New-Object System.Drawing.PointF(540, 784)
$tailNotch = New-Object System.Drawing.PointF(470, 554)

# 4. 整体柔和环境光遮挡阴影
for ($i = 1; $i -le 14; $i++) {
    $alpha = [int](26 - $i * 1.6)
    if ($alpha -gt 0) {
        $dy = $i * 3.5
        $shPts = @(
            (New-Object System.Drawing.PointF($tip.X, ($tip.Y + $dy))),
            (New-Object System.Drawing.PointF($rightWing.X, ($rightWing.Y + $dy))),
            (New-Object System.Drawing.PointF($tailNotch.X, ($tailNotch.Y + $dy))),
            (New-Object System.Drawing.PointF($leftWing.X, ($leftWing.Y + $dy)))
        )
        $shBrush = New-Object System.Drawing.SolidBrush([System.Drawing.Color]::FromArgb($alpha, 0, 15, 45))
        $g.FillPolygon($shBrush, $shPts)
    }
}

# 5. 左翼（受光面：纯白陶瓷质感）
$leftPts = @($tip, $leftWing, $tailNotch)
$leftGrad = New-Object System.Drawing.Drawing2D.LinearGradientBrush(
    $leftWing, $tip,
    [System.Drawing.Color]::FromArgb(255, 0xF0, 0xF5, 0xFA),
    [System.Drawing.Color]::FromArgb(255, 0xFF, 0xFF, 0xFF))
$g.FillPolygon($leftGrad, $leftPts)

# 6. 右翼（侧背光面：微透晶莹青白质感）
$rightPts = @($tip, $tailNotch, $rightWing)
$rightGrad = New-Object System.Drawing.Drawing2D.LinearGradientBrush(
    $tailNotch, $rightWing,
    [System.Drawing.Color]::FromArgb(255, 0x00, 0xDF, 0xE8), # 电光青
    [System.Drawing.Color]::FromArgb(255, 0xC4, 0xF5, 0xFA)) # 浅青白
$g.FillPolygon($rightGrad, $rightPts)

# 7. 中轴脊线与边缘高光
$spinePen = New-Object System.Drawing.Pen([System.Drawing.Color]::FromArgb(200, 255, 255, 255), 4)
$g.DrawLine($spinePen, $tailNotch, $tip)

# 8. 尾部微光推进线（强化动势感）
$trailPen = New-Object System.Drawing.Pen([System.Drawing.Color]::FromArgb(180, 255, 255, 255), 24)
$trailPen.StartCap = [System.Drawing.Drawing2D.LineCap]::Round
$trailPen.EndCap = [System.Drawing.Drawing2D.LineCap]::Round
$g.DrawLine($trailPen, 340, 684, 400, 624)

$g.Dispose()

# ---------- 缩放函数 ----------
function Resize([System.Drawing.Bitmap]$src, [int]$size) {
    $b = New-Object System.Drawing.Bitmap($size, $size)
    $gg = [System.Drawing.Graphics]::FromImage($b)
    $gg.InterpolationMode = [System.Drawing.Drawing2D.InterpolationMode]::HighQualityBicubic
    $gg.SmoothingMode = [System.Drawing.Drawing2D.SmoothingMode]::AntiAlias
    $gg.PixelOffsetMode = [System.Drawing.Drawing2D.PixelOffsetMode]::HighQuality
    $gg.DrawImage($src, (New-Object System.Drawing.Rectangle(0, 0, $size, $size)))
    $gg.Dispose()
    return $b
}

# ---------- BMP(DIB) 与 PNG 打包为 ICO ----------
function BitmapToDib([System.Drawing.Bitmap]$b) {
    $w = $b.Width; $h = $b.Height
    $ms = New-Object System.IO.MemoryStream
    $bw = New-Object System.IO.BinaryWriter($ms)
    $bw.Write([UInt32]40); $bw.Write([Int32]$w); $bw.Write([Int32]($h * 2))
    $bw.Write([UInt16]1); $bw.Write([UInt16]32); $bw.Write([UInt32]0)
    $bw.Write([UInt32]($w * $h * 4)); $bw.Write([Int32]0); $bw.Write([Int32]0)
    $bw.Write([UInt32]0); $bw.Write([UInt32]0)
    for ($y = $h - 1; $y -ge 0; $y--) {
        for ($x = 0; $x -lt $w; $x++) {
            $c = $b.GetPixel($x, $y)
            $bw.Write($c.B); $bw.Write($c.G); $bw.Write($c.R); $bw.Write($c.A)
        }
    }
    $rowBytes = [int][Math]::Ceiling($w / 8.0)
    $pad = (4 - ($rowBytes % 4)) % 4
    $row = New-Object byte[] ($rowBytes + $pad)
    for ($y = $h - 1; $y -ge 0; $y--) {
        [Array]::Clear($row, 0, $row.Length)
        for ($x = 0; $x -lt $w; $x++) {
            if ($b.GetPixel($x, $y).A -eq 0) {
                $row[[int][Math]::Floor($x / 8)] = $row[[int][Math]::Floor($x / 8)] -bor (0x80 -shr ($x % 8))
            }
        }
        $bw.Write($row)
    }
    $bw.Flush()
    return $ms.ToArray()
}

function BitmapToPng([System.Drawing.Bitmap]$b) {
    $ms = New-Object System.IO.MemoryStream
    $b.Save($ms, [System.Drawing.Imaging.ImageFormat]::Png)
    return $ms.ToArray()
}

$sizes = @(256, 128, 64, 48, 32, 24, 16)
$entries = @()
foreach ($s in $sizes) {
    $b = Resize $bmp $s
    if ($s -eq 256) { $entries += , @{ size = $s; data = [byte[]](BitmapToPng $b) } }
    else { $entries += , @{ size = $s; data = [byte[]](BitmapToDib $b) } }
    $b.Dispose()
}

$ico = New-Object System.IO.MemoryStream
$iw = New-Object System.IO.BinaryWriter($ico)
$iw.Write([UInt16]0); $iw.Write([UInt16]1); $iw.Write([UInt16]$entries.Count)
$offset = 6 + 16 * $entries.Count
foreach ($e in $entries) {
    $sz = $e.size
    $iw.Write([Byte]($(if ($sz -ge 256) { 0 } else { $sz })))
    $iw.Write([Byte]($(if ($sz -ge 256) { 0 } else { $sz })))
    $iw.Write([Byte]0); $iw.Write([Byte]0)
    $iw.Write([UInt16]1); $iw.Write([UInt16]32)
    $iw.Write([UInt32]$e.data.Length)
    $iw.Write([UInt32]$offset)
    $offset += $e.data.Length
}
foreach ($e in $entries) { $iw.Write($e.data) }
$iw.Flush()
$icoDir = Split-Path -Parent $OutIco
if (-not (Test-Path $icoDir)) { New-Item -ItemType Directory -Path $icoDir | Out-Null }
[IO.File]::WriteAllBytes($OutIco, $ico.ToArray())
"icon.ico written: $($ico.Length) bytes, sizes: $($sizes -join '/')"

# ---------- 导出 Web Favicon (SVG 矢量 + 192px PNG) ----------
$svgContent = @"
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1024 1024">
  <defs>
    <linearGradient id="bgGrad" x1="7.8%" y1="4.9%" x2="92.2%" y2="95.1%">
      <stop offset="0%" stop-color="#1E48FA"/>
      <stop offset="100%" stop-color="#0284C7"/>
    </linearGradient>
    <linearGradient id="leftWingGrad" x1="240" y1="484" x2="764" y2="260" gradientUnits="userSpaceOnUse">
      <stop offset="0%" stop-color="#F0F5FA"/>
      <stop offset="100%" stop-color="#FFFFFF"/>
    </linearGradient>
    <linearGradient id="rightWingGrad" x1="470" y1="554" x2="540" y2="784" gradientUnits="userSpaceOnUse">
      <stop offset="0%" stop-color="#00DFE8"/>
      <stop offset="100%" stop-color="#C4F5FA"/>
    </linearGradient>
    <filter id="softShadow" x="-20%" y="-20%" width="140%" height="140%">
      <feDropShadow dx="0" dy="22" stdDeviation="18" flood-color="#000F2D" flood-opacity="0.32"/>
    </filter>
  </defs>

  <!-- 1. Background Tile -->
  <rect x="48" y="48" width="928" height="928" rx="220" fill="url(#bgGrad)"/>
  <rect x="52" y="52" width="920" height="920" rx="218" fill="none" stroke="#FFFFFF" stroke-opacity="0.24" stroke-width="5"/>

  <!-- 2. Origami Dart with Soft Shadow -->
  <g filter="url(#softShadow)">
    <!-- Trail Line -->
    <line x1="340" y1="684" x2="400" y2="624" stroke="#FFFFFF" stroke-opacity="0.7" stroke-width="24" stroke-linecap="round"/>
    <!-- Left Wing (White Porcelain) -->
    <polygon points="764,260 240,484 470,554" fill="url(#leftWingGrad)"/>
    <!-- Right Wing (Electric Cyan) -->
    <polygon points="764,260 470,554 540,784" fill="url(#rightWingGrad)"/>
    <!-- Spine Ridge Line -->
    <line x1="470" y1="554" x2="764" y2="260" stroke="#FFFFFF" stroke-opacity="0.8" stroke-width="4"/>
  </g>
</svg>
"@
[IO.File]::WriteAllText($OutSvg, $svgContent, [System.Text.Encoding]::UTF8)
"favicon.svg written: $OutSvg"

$favPng = Resize $bmp 192
[IO.File]::WriteAllBytes($OutPng, (BitmapToPng $favPng))
$favPng.Dispose()
"favicon.png written: $OutPng (192x192)"

# ---------- 预览图（深/浅底展示 + 任务栏全尺寸模拟） ----------
$pw = 800; $ph = 360
$prev = New-Object System.Drawing.Bitmap($pw, $ph)
$pg = [System.Drawing.Graphics]::FromImage($prev)
$pg.Clear([System.Drawing.Color]::FromArgb(255, 0x1A, 0x1D, 0x24))
$pg.InterpolationMode = [System.Drawing.Drawing2D.InterpolationMode]::HighQualityBicubic

$pg.DrawImage($bmp, (New-Object System.Drawing.Rectangle(40, 40, 220, 220)))

$lightRect = New-Object System.Drawing.Rectangle(290, 40, 220, 220)
$pg.FillRectangle([System.Drawing.Brushes]::White, $lightRect)
$pg.DrawImage($bmp, $lightRect)

$font = New-Object System.Drawing.Font("Segoe UI", 9, [System.Drawing.FontStyle]::Bold)
$dimBrush = New-Object System.Drawing.SolidBrush([System.Drawing.Color]::FromArgb(180, 160, 170, 185))
$pg.DrawString("Dark Tray (48/32/24/16)", $font, $dimBrush, 540, 40)
$pg.DrawString("Light Tray (48/32/24/16)", $font, $dimBrush, 540, 160)

$trayDarkX = 540
foreach ($s in @(48, 32, 24, 16)) {
    $sb = Resize $bmp $s
    $pg.DrawImage($sb, (New-Object System.Drawing.Rectangle($trayDarkX, 68, $s, $s)))
    $trayDarkX += $s + 16
    $sb.Dispose()
}

$trayLightX = 540
foreach ($s in @(48, 32, 24, 16)) {
    $sb = Resize $bmp $s
    $bgRect = New-Object System.Drawing.Rectangle($trayLightX, 188, $s, $s)
    $pg.FillRectangle([System.Drawing.Brushes]::White, $bgRect)
    $pg.DrawImage($sb, $bgRect)
    $trayLightX += $s + 16
    $sb.Dispose()
}

$pg.Dispose()
[IO.File]::WriteAllBytes($OutPreview, (BitmapToPng $prev))
"preview written: $OutPreview"
$bmp.Dispose()
