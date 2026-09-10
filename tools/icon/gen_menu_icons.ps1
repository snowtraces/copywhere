# 生成托盘菜单专用 16x16 / 32x32 ICO 图标（折纸光翼专属主题）
# 用法: pwsh -File tools/icon/gen_menu_icons.ps1
param(
    [string]$OutDir = "$PSScriptRoot\..\..\internal\webui\icons",
    [string]$OutPreview = "$PSScriptRoot\menu_preview.png"
)
Set-StrictMode -Version Latest
Add-Type -AssemblyName System.Drawing

if (-not (Test-Path $OutDir)) { New-Item -ItemType Directory -Path $OutDir | Out-Null }

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

function Save-Ico([System.Drawing.Bitmap]$bmp, [string]$outPath) {
    $sizes = @(32, 16)
    $entries = @()
    foreach ($s in $sizes) {
        $b = New-Object System.Drawing.Bitmap($s, $s)
        $g = [System.Drawing.Graphics]::FromImage($b)
        $g.InterpolationMode = [System.Drawing.Drawing2D.InterpolationMode]::HighQualityBicubic
        $g.SmoothingMode = [System.Drawing.Drawing2D.SmoothingMode]::AntiAlias
        $g.PixelOffsetMode = [System.Drawing.Drawing2D.PixelOffsetMode]::HighQuality
        $g.DrawImage($bmp, (New-Object System.Drawing.Rectangle(0, 0, $s, $s)))
        $g.Dispose()
        $entries += , @{ size = $s; data = [byte[]](BitmapToDib $b) }
        $b.Dispose()
    }
    $ico = New-Object System.IO.MemoryStream
    $iw = New-Object System.IO.BinaryWriter($ico)
    $iw.Write([UInt16]0); $iw.Write([UInt16]1); $iw.Write([UInt16]$entries.Count)
    $offset = 6 + 16 * $entries.Count
    foreach ($e in $entries) {
        $sz = $e.size
        $iw.Write([Byte]$sz); $iw.Write([Byte]$sz); $iw.Write([Byte]0); $iw.Write([Byte]0)
        $iw.Write([UInt16]1); $iw.Write([UInt16]32)
        $iw.Write([UInt32]$e.data.Length); $iw.Write([UInt32]$offset)
        $offset += $e.data.Length
    }
    foreach ($e in $entries) { $iw.Write($e.data) }
    $iw.Flush()
    [IO.File]::WriteAllBytes($outPath, $ico.ToArray())
}

$S = 128

function Draw-Base([System.Drawing.Graphics]$g, [System.Drawing.Color]$c1, [System.Drawing.Color]$c2) {
    $m = 6.0; $r = 28.0
    $tile = New-RoundedRect $m $m ($S - 2*$m) ($S - 2*$m) $r
    $p1 = New-Object System.Drawing.PointF(10.0, 6.0)
    $p2 = New-Object System.Drawing.PointF(118.0, 122.0)
    $grad = New-Object System.Drawing.Drawing2D.LinearGradientBrush($p1, $p2, $c1, $c2)
    $g.FillPath($grad, $tile)
    # 内边缘微光
    $innerRim = New-RoundedRect ($m + 1.5) ($m + 1.5) ($S - 2*$m - 3) ($S - 2*$m - 3) ($r - 1)
    $rimPen = New-Object System.Drawing.Pen([System.Drawing.Color]::FromArgb(65, 255, 255, 255), 2.5)
    $g.DrawPath($rimPen, $innerRim)
}

# 1. 打开面板 (menu_open.ico) - 折纸光翼控制台 (深空科技蓝 -> 电光青蓝)
$bOpen = New-Object System.Drawing.Bitmap($S, $S)
$g = [System.Drawing.Graphics]::FromImage($bOpen)
$g.SmoothingMode = [System.Drawing.Drawing2D.SmoothingMode]::AntiAlias
$g.PixelOffsetMode = [System.Drawing.Drawing2D.PixelOffsetMode]::HighQuality
Draw-Base $g ([System.Drawing.Color]::FromArgb(255, 0x1E, 0x48, 0xFA)) ([System.Drawing.Color]::FromArgb(255, 0x02, 0x84, 0xC7))
# 折纸飞梭 (东北 45°)
$tip = New-Object System.Drawing.PointF(96, 32)
$lWing = New-Object System.Drawing.PointF(32, 60)
$rWing = New-Object System.Drawing.PointF(68, 96)
$notch = New-Object System.Drawing.PointF(59, 69)
# 左翼受光面（白瓷）
$lPts = @($tip, $lWing, $notch)
$g.FillPolygon([System.Drawing.Brushes]::White, $lPts)
# 右翼背光面（电光青）
$rPts = @($tip, $notch, $rWing)
$rBrush = New-Object System.Drawing.SolidBrush([System.Drawing.Color]::FromArgb(255, 0x00, 0xDF, 0xE8))
$g.FillPolygon($rBrush, $rPts)
# 尾迹光束
$trPen = New-Object System.Drawing.Pen([System.Drawing.Color]::FromArgb(200, 255, 255, 255), 4)
$trPen.StartCap = [System.Drawing.Drawing2D.LineCap]::Round
$trPen.EndCap = [System.Drawing.Drawing2D.LineCap]::Round
$g.DrawLine($trPen, 42, 86, 50, 78)
$g.Dispose()
Save-Ico $bOpen (Join-Path $OutDir "menu_open.ico")

# 2. 暂停自动同步 (menu_pause.ico) - 琥珀金切面双柱 (Laser Amber -> Warm Gold)
$bPause = New-Object System.Drawing.Bitmap($S, $S)
$g = [System.Drawing.Graphics]::FromImage($bPause)
$g.SmoothingMode = [System.Drawing.Drawing2D.SmoothingMode]::AntiAlias
$g.PixelOffsetMode = [System.Drawing.Drawing2D.PixelOffsetMode]::HighQuality
Draw-Base $g ([System.Drawing.Color]::FromArgb(255, 0xD9, 0x77, 0x06)) ([System.Drawing.Color]::FromArgb(255, 0xF5, 0x9E, 0x0B))
# 折纸风格双暂停条 (圆角矩形，纯白瓷光)
$p1Rect = New-RoundedRect 36 30 18 68 5
$p2Rect = New-RoundedRect 74 30 18 68 5
$g.FillPath([System.Drawing.Brushes]::White, $p1Rect)
$g.FillPath((New-Object System.Drawing.SolidBrush([System.Drawing.Color]::FromArgb(240, 255, 255, 255))), $p2Rect)
$g.Dispose()
Save-Ico $bPause (Join-Path $OutDir "menu_pause.ico")

# 3. 恢复自动同步 (menu_resume.ico) - 极光翡翠折纸启航 (Aurora Emerald -> Vivid Mint)
$bResume = New-Object System.Drawing.Bitmap($S, $S)
$g = [System.Drawing.Graphics]::FromImage($bResume)
$g.SmoothingMode = [System.Drawing.Drawing2D.SmoothingMode]::AntiAlias
$g.PixelOffsetMode = [System.Drawing.Drawing2D.PixelOffsetMode]::HighQuality
Draw-Base $g ([System.Drawing.Color]::FromArgb(255, 0x05, 0x96, 0x69)) ([System.Drawing.Color]::FromArgb(255, 0x10, 0xB9, 0x81))
# 水平启航折纸光翼 (指向右方 0°)
$rTip = New-Object System.Drawing.PointF(102, 64)
$rTop = New-Object System.Drawing.PointF(36, 28)
$rBtm = New-Object System.Drawing.PointF(36, 100)
$rNotch = New-Object System.Drawing.PointF(56, 64)
# 上翼纯白受光面
$g.FillPolygon([System.Drawing.Brushes]::White, @($rTip, $rTop, $rNotch))
# 下翼微青透光面
$resCyan = New-Object System.Drawing.SolidBrush([System.Drawing.Color]::FromArgb(255, 0x80, 0xF5, 0xEA))
$g.FillPolygon($resCyan, @($rTip, $rNotch, $rBtm))
# 中脊高光
$spPen = New-Object System.Drawing.Pen([System.Drawing.Color]::FromArgb(180, 255, 255, 255), 2)
$g.DrawLine($spPen, 56, 64, 102, 64)
$g.Dispose()
Save-Ico $bResume (Join-Path $OutDir "menu_resume.ico")

# 4. 打开接收目录 (menu_folder.ico) - 电光蓝折纸收纳仓 (Electric Sky -> Cyan Blue)
$bFolder = New-Object System.Drawing.Bitmap($S, $S)
$g = [System.Drawing.Graphics]::FromImage($bFolder)
$g.SmoothingMode = [System.Drawing.Drawing2D.SmoothingMode]::AntiAlias
$g.PixelOffsetMode = [System.Drawing.Drawing2D.PixelOffsetMode]::HighQuality
Draw-Base $g ([System.Drawing.Color]::FromArgb(255, 0x02, 0x84, 0xC7)) ([System.Drawing.Color]::FromArgb(255, 0x00, 0xDF, 0xE8))
# 折纸文件夹外壳 (纯白主仓 + 电光青卡片)
$fPath = New-Object System.Drawing.Drawing2D.GraphicsPath
$fPath.AddLine(26, 42, 54, 42)
$fPath.AddLine(62, 52, 102, 52)
$fPath.AddLine(102, 94, 26, 94)
$fPath.CloseFigure()
$g.FillPath([System.Drawing.Brushes]::White, $fPath)
# 文件夹内露出的折角数据卡片
$cPath = New-Object System.Drawing.Drawing2D.GraphicsPath
$cPath.AddLine(38, 30, 78, 30)
$cPath.AddLine(90, 42, 90, 68)
$cPath.AddLine(38, 68, 38, 30)
$cPath.CloseFigure()
$cardBrush = New-Object System.Drawing.SolidBrush([System.Drawing.Color]::FromArgb(240, 0x1E, 0x48, 0xFA))
$g.FillPath($cardBrush, $cPath)
# 浮雕折线
$foldPen = New-Object System.Drawing.Pen([System.Drawing.Color]::FromArgb(180, 0x02, 0x84, 0xC7), 2)
$g.DrawLine($foldPen, 26, 58, 102, 58)
$g.Dispose()
Save-Ico $bFolder (Join-Path $OutDir "menu_folder.ico")

# 5. 退出 (menu_quit.ico) - 霓虹洋红折纸关闭信标 (Crimson Rose -> Deep Rose)
$bQuit = New-Object System.Drawing.Bitmap($S, $S)
$g = [System.Drawing.Graphics]::FromImage($bQuit)
$g.SmoothingMode = [System.Drawing.Drawing2D.SmoothingMode]::AntiAlias
$g.PixelOffsetMode = [System.Drawing.Drawing2D.PixelOffsetMode]::HighQuality
Draw-Base $g ([System.Drawing.Color]::FromArgb(255, 0xE1, 0x1D, 0x48)) ([System.Drawing.Color]::FromArgb(255, 0xF4, 0x3F, 0x5E))
# 几何电源断开环
$pPen = New-Object System.Drawing.Pen([System.Drawing.Brushes]::White, 10)
$pPen.StartCap = [System.Drawing.Drawing2D.LineCap]::Round
$pPen.EndCap = [System.Drawing.Drawing2D.LineCap]::Round
$g.DrawArc($pPen, 26, 26, 76, 76, 132, 276)
# 核心垂直激光电极
$g.DrawLine($pPen, 64, 18, 64, 56)
$g.Dispose()
Save-Ico $bQuit (Join-Path $OutDir "menu_quit.ico")

# 6. 暂停同步状态的主托盘图标 (icon_paused.ico)
$bPauseTray = New-Object System.Drawing.Bitmap($S, $S)
$g = [System.Drawing.Graphics]::FromImage($bPauseTray)
$g.SmoothingMode = [System.Drawing.Drawing2D.SmoothingMode]::AntiAlias
$g.PixelOffsetMode = [System.Drawing.Drawing2D.PixelOffsetMode]::HighQuality
Draw-Base $g ([System.Drawing.Color]::FromArgb(255, 0x1E, 0x30, 0x60)) ([System.Drawing.Color]::FromArgb(255, 0x2A, 0x40, 0x70)) # 沉着待机深蓝
# 待机灰白光翼
$g.FillPolygon((New-Object System.Drawing.SolidBrush([System.Drawing.Color]::FromArgb(200, 255, 255, 255))), @($tip, $lWing, $notch))
$g.FillPolygon((New-Object System.Drawing.SolidBrush([System.Drawing.Color]::FromArgb(160, 160, 190, 220))), @($tip, $notch, $rWing))
# 右下角醒目的琥珀金暂停标记徽章
$bRect = New-RoundedRect 70 70 52 52 14
$g.FillPath((New-Object System.Drawing.SolidBrush([System.Drawing.Color]::FromArgb(255, 0xD9, 0x77, 0x06))), $bRect)
$g.DrawPath((New-Object System.Drawing.Pen([System.Drawing.Brushes]::White, 3)), $bRect)
# 徽章内的暂停双竖线
$g.FillRectangle([System.Drawing.Brushes]::White, 83, 82, 8, 28)
$g.FillRectangle([System.Drawing.Brushes]::White, 97, 82, 8, 28)
$g.Dispose()
Save-Ico $bPauseTray (Join-Path $OutDir "icon_paused.ico")

# ---------- 生成横向综合预览图 (menu_preview.png) ----------
$pw = 720; $ph = 260
$prev = New-Object System.Drawing.Bitmap($pw, $ph)
$pg = [System.Drawing.Graphics]::FromImage($prev)
$pg.Clear([System.Drawing.Color]::FromArgb(255, 0x0F, 0x17, 0x2A)) # Slate 900
$pg.InterpolationMode = [System.Drawing.Drawing2D.InterpolationMode]::HighQualityBicubic

$font = New-Object System.Drawing.Font("Segoe UI", 9, [System.Drawing.FontStyle]::Bold)
$titleFont = New-Object System.Drawing.Font("Segoe UI", 12, [System.Drawing.FontStyle]::Bold)
$dimBrush = New-Object System.Drawing.SolidBrush([System.Drawing.Color]::FromArgb(200, 0x84, 0x96, 0xB4))
$whiteBrush = [System.Drawing.Brushes]::White

$pg.DrawString("copywhere 系统托盘折纸光翼主题图标套件 (32px / 16px 系统托盘渲染)", $titleFont, $whiteBrush, 24, 18)

$items = @(
    @{ Name = "打开面板"; Bmp = $bOpen },
    @{ Name = "暂停同步"; Bmp = $bPause },
    @{ Name = "恢复同步"; Bmp = $bResume },
    @{ Name = "接收目录"; Bmp = $bFolder },
    @{ Name = "退出程序"; Bmp = $bQuit },
    @{ Name = "托盘暂停态"; Bmp = $bPauseTray }
)

$startX = 24
foreach ($it in $items) {
    # 54px 展示
    $pg.DrawImage($it.Bmp, (New-Object System.Drawing.Rectangle($startX, 58, 54, 54)))
    # 32px
    $pg.DrawImage($it.Bmp, (New-Object System.Drawing.Rectangle(($startX + 62), 62, 32, 32)))
    # 16px (原生托盘大小)
    $pg.DrawImage($it.Bmp, (New-Object System.Drawing.Rectangle(($startX + 62), 98, 16, 16)))
    # 文字标签
    $pg.DrawString($it.Name, $font, $dimBrush, $startX, 126)
    $startX += 114
}

# 浅色菜单背景对比条
$lightBgRect = New-Object System.Drawing.Rectangle(24, 160, 672, 80)
$pg.FillRectangle((New-Object System.Drawing.SolidBrush([System.Drawing.Color]::FromArgb(255, 0xF3, 0xF6, 0xFB))), $lightBgRect)
$pg.DrawRectangle((New-Object System.Drawing.Pen([System.Drawing.Color]::FromArgb(255, 0xE1, 0xE7, 0xF2), 1)), $lightBgRect)

$menuFont = New-Object System.Drawing.Font("Segoe UI", 9)
$darkText = New-Object System.Drawing.SolidBrush([System.Drawing.Color]::FromArgb(255, 0x0D, 0x15, 0x27))

$menuX = 36
foreach ($it in $items) {
    $pg.DrawImage($it.Bmp, (New-Object System.Drawing.Rectangle($menuX, 192, 16, 16)))
    $pg.DrawString($it.Name, $menuFont, $darkText, ($menuX + 22), 190)
    $menuX += 110
}

$pg.Dispose()
$prev.Save($OutPreview, [System.Drawing.Imaging.ImageFormat]::Png)
$prev.Dispose()

$bOpen.Dispose(); $bPause.Dispose(); $bResume.Dispose(); $bFolder.Dispose(); $bQuit.Dispose(); $bPauseTray.Dispose()

Write-Output "Tray Origami icons generated in $OutDir"
Write-Output "Preview saved to $OutPreview"
