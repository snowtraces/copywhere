# 生成托盘菜单专用 16x16 / 32x32 ICO 图标（浅色线框风格）。
# 与 WebUI 浅色主题一致：浅纸底 + 1px 深灰描边 + 深墨线稿 + 状态色点缀。
# 主体应用图标不受本脚本影响（见 gen_icon.ps1，折纸光翼主题）。
# 用法: pwsh -File tools/icon/gen_menu_icons.ps1
param(
    [string]$OutDir = "$PSScriptRoot\..\..\internal\webui\icons",
    [string]$OutPreview = "$PSScriptRoot\menu_preview.png"
)
Set-StrictMode -Version Latest
Add-Type -AssemblyName System.Drawing

$TileBG = [System.Drawing.Color]::FromArgb(255, 0xFB, 0xFA, 0xF5) # 浅纸底
$Border = [System.Drawing.Color]::FromArgb(255, 0x7E, 0x7C, 0x75) # 浅色主题描边灰
$Ink    = [System.Drawing.Color]::FromArgb(255, 0x1D, 0x25, 0x2C) # 深墨线稿
$InkDim = [System.Drawing.Color]::FromArgb(110, 0x1D, 0x25, 0x2C) # 半透墨
$Teal   = [System.Drawing.Color]::FromArgb(255, 0x05, 0x6E, 0x7A) # accent 墨青
$Green  = [System.Drawing.Color]::FromArgb(255, 0x09, 0x6A, 0x3D) # 恢复绿
$Red    = [System.Drawing.Color]::FromArgb(255, 0xAB, 0x24, 0x40) # 退出红
$Amber  = [System.Drawing.Color]::FromArgb(255, 0xD9, 0x77, 0x06) # 暂停琥珀

if (-not (Test-Path $OutDir)) { New-Item -ItemType Directory -Path $OutDir | Out-Null }

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

# 浅色线框底板：浅纸底满幅方块 + 深灰细描边（方角、无渐变）
function Draw-Base([System.Drawing.Graphics]$g) {
    $g.Clear($TileBG)
    $framePen = New-Object System.Drawing.Pen($Border, 5)
    $g.DrawRectangle($framePen, 3.5, 3.5, $S - 7, $S - 7)
    $framePen.Dispose()
}

function New-Pen([System.Drawing.Color]$c, [float]$w) {
    $p = New-Object System.Drawing.Pen($c, $w)
    $p.StartCap = [System.Drawing.Drawing2D.LineCap]::Square
    $p.EndCap = [System.Drawing.Drawing2D.LineCap]::Square
    $p.LineJoin = [System.Drawing.Drawing2D.LineJoin]::Miter
    return $p
}

function New-GlyphBitmap {
    $b = New-Object System.Drawing.Bitmap($S, $S)
    $g = [System.Drawing.Graphics]::FromImage($b)
    $g.SmoothingMode = [System.Drawing.Drawing2D.SmoothingMode]::AntiAlias
    $g.PixelOffsetMode = [System.Drawing.Drawing2D.PixelOffsetMode]::HighQuality
    Draw-Base $g
    return $b, $g
}

# 1. 打开面板 (menu_open.ico) - 纸飞机线稿（深墨）+ 墨青尾迹
$bOpen, $g = New-GlyphBitmap
$dartPen = New-Pen $Ink 7
$g.DrawPolygon($dartPen, [System.Drawing.PointF[]]@(
    [System.Drawing.PointF]::new(96, 32),
    [System.Drawing.PointF]::new(30, 62),
    [System.Drawing.PointF]::new(59, 71),
    [System.Drawing.PointF]::new(69, 100)
))
$spPen = New-Pen $InkDim 3
$g.DrawLine($spPen, 59, 71, 96, 32)
$trPen = New-Pen $Teal 7
$g.DrawLine($trPen, 38, 90, 47, 81)
$dartPen.Dispose(); $spPen.Dispose(); $trPen.Dispose()
$g.Dispose()
Save-Ico $bOpen (Join-Path $OutDir "menu_open.ico")

# 2. 暂停自动同步 (menu_pause.ico) - 深墨双竖条
$bPause, $g = New-GlyphBitmap
$inkBrush = New-Object System.Drawing.SolidBrush($Ink)
$g.FillRectangle($inkBrush, 40, 36, 15, 56)
$g.FillRectangle($inkBrush, 73, 36, 15, 56)
$inkBrush.Dispose()
$g.Dispose()
Save-Ico $bPause (Join-Path $OutDir "menu_pause.ico")

# 3. 恢复自动同步 (menu_resume.ico) - 墨青右向实心三角
$bResume, $g = New-GlyphBitmap
$resBrush = New-Object System.Drawing.SolidBrush($Green)
$g.FillPolygon($resBrush, [System.Drawing.PointF[]]@(
    [System.Drawing.PointF]::new(46, 34),
    [System.Drawing.PointF]::new(46, 94),
    [System.Drawing.PointF]::new(98, 64)
))
$resBrush.Dispose()
$g.Dispose()
Save-Ico $bResume (Join-Path $OutDir "menu_resume.ico")

# 4. 打开接收目录 (menu_folder.ico) - 深墨文件夹线框
$bFolder, $g = New-GlyphBitmap
$foldPen = New-Pen $Ink 7
$fPath = New-Object System.Drawing.Drawing2D.GraphicsPath
$fPath.AddLines([System.Drawing.PointF[]]@(
    [System.Drawing.PointF]::new(24, 44),
    [System.Drawing.PointF]::new(54, 44),
    [System.Drawing.PointF]::new(63, 54),
    [System.Drawing.PointF]::new(104, 54),
    [System.Drawing.PointF]::new(104, 96),
    [System.Drawing.PointF]::new(24, 96)
))
$fPath.CloseFigure()
$g.DrawPath($foldPen, $fPath)
$fPath.Dispose()
$foldPen.Dispose()
$g.Dispose()
Save-Ico $bFolder (Join-Path $OutDir "menu_folder.ico")

# 5. 退出 (menu_quit.ico) - 深红电源符号
$bQuit, $g = New-GlyphBitmap
$pwPen = New-Pen $Red 8
$g.DrawArc($pwPen, 28, 30, 72, 72, 130, 280)
$g.DrawLine($pwPen, 64, 20, 64, 58)
$pwPen.Dispose()
$g.Dispose()
Save-Ico $bQuit (Join-Path $OutDir "menu_quit.ico")

# 6. 暂停同步状态的主托盘图标 (icon_paused.ico) - 浅底 + 半透纸飞机 + 琥珀暂停徽章
#    注意：主托盘常态图标用 gen_icon.ps1 的折纸设计，本枚仅是"已暂停"状态角标版。
$bPauseTray, $g = New-GlyphBitmap
$dimDart = New-Pen $InkDim 7
$g.DrawPolygon($dimDart, [System.Drawing.PointF[]]@(
    [System.Drawing.PointF]::new(96, 32),
    [System.Drawing.PointF]::new(30, 62),
    [System.Drawing.PointF]::new(59, 71),
    [System.Drawing.PointF]::new(69, 100)
))
$dimDart.Dispose()
# 右下角方形琥珀暂停徽章（直角硬边 + 深墨双杠）
$badgeBrush = New-Object System.Drawing.SolidBrush($Amber)
$g.FillRectangle($badgeBrush, 74, 74, 44, 44)
$badgeBrush.Dispose()
$barDark = New-Object System.Drawing.SolidBrush($TileBG)
$g.FillRectangle($barDark, 86, 84, 7, 24)
$g.FillRectangle($barDark, 99, 84, 7, 24)
$barDark.Dispose()
$g.Dispose()
Save-Ico $bPauseTray (Join-Path $OutDir "icon_paused.ico")

# ---------- 生成横向综合预览图 ----------
$pw = 720; $ph = 260
$prev = New-Object System.Drawing.Bitmap($pw, $ph)
$pg = [System.Drawing.Graphics]::FromImage($prev)
$pg.Clear([System.Drawing.Color]::FromArgb(255, 0xED, 0xEB, 0xE3)) # 浅纸背景
$pg.InterpolationMode = [System.Drawing.Drawing2D.InterpolationMode]::HighQualityBicubic

$font = New-Object System.Drawing.Font("Consolas", 9, [System.Drawing.FontStyle]::Bold)
$titleFont = New-Object System.Drawing.Font("Consolas", 12, [System.Drawing.FontStyle]::Bold)
$dimBrush = New-Object System.Drawing.SolidBrush([System.Drawing.Color]::FromArgb(220, 0x4F, 0x5A, 0x64))
$inkBrush = New-Object System.Drawing.SolidBrush([System.Drawing.Color]::FromArgb(255, 0x16, 0x1D, 0x23))

$pg.DrawString("copywhere tray menu icons - LIGHT WIREFRAME (32px / 16px)", $titleFont, $inkBrush, 24, 18)

$items = @(
    @{ Name = "open";     Bmp = $bOpen },
    @{ Name = "pause";    Bmp = $bPause },
    @{ Name = "resume";   Bmp = $bResume },
    @{ Name = "folder";   Bmp = $bFolder },
    @{ Name = "quit";     Bmp = $bQuit },
    @{ Name = "paused";   Bmp = $bPauseTray }
)

$startX = 24
foreach ($it in $items) {
    $pg.DrawImage($it.Bmp, (New-Object System.Drawing.Rectangle($startX, 58, 54, 54)))
    $pg.DrawImage($it.Bmp, (New-Object System.Drawing.Rectangle(($startX + 62), 62, 32, 32)))
    $pg.DrawImage($it.Bmp, (New-Object System.Drawing.Rectangle(($startX + 62), 98, 16, 16)))
    $pg.DrawString($it.Name, $font, $dimBrush, $startX, 126)
    $startX += 114
}

# 深色菜单背景对比条（验证浅色图标在深色菜单下的可见性）
$darkBgRect = New-Object System.Drawing.Rectangle(24, 160, 672, 80)
$pg.FillRectangle((New-Object System.Drawing.SolidBrush([System.Drawing.Color]::FromArgb(255, 0x0A, 0x0E, 0x13))), $darkBgRect)
$pg.DrawRectangle((New-Object System.Drawing.Pen([System.Drawing.Color]::FromArgb(255, 0x26, 0x32, 0x3E), 1)), $darkBgRect)

$menuX = 36
foreach ($it in $items) {
    $pg.DrawImage($it.Bmp, (New-Object System.Drawing.Rectangle($menuX, 192, 16, 16)))
    $pg.DrawString($it.Name, $font, $dimBrush, ($menuX + 22), 190)
    $menuX += 110
}

$pg.Dispose()
$prev.Save($OutPreview, [System.Drawing.Imaging.ImageFormat]::Png)
$prev.Dispose()

$bOpen.Dispose(); $bPause.Dispose(); $bResume.Dispose(); $bFolder.Dispose(); $bQuit.Dispose(); $bPauseTray.Dispose()

Write-Output "Tray menu LIGHT WIREFRAME icons generated in $OutDir"
Write-Output "Preview saved to $OutPreview"
