//go:build ignore

// 从 internal/wailsui/frontend/favicon.png 生成 MSIX 清单引用的图块资源。
// 生成结果随仓库提交，构建 MSIX 时直接复制，不需要额外的图像库。
// 用法（仓库根目录）：go run tools/msix/gen_assets.go
package main

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"os"
	"path/filepath"
)

type target struct {
	name string
	w, h int
}

var targets = []target{
	{"StoreLogo.png", 50, 50},
	{"Square44x44Logo.png", 44, 44},
	{"Square71x71Logo.png", 71, 71},
	{"Square150x150Logo.png", 150, 150},
	{"Square310x310Logo.png", 310, 310},
}

func main() {
	srcPath := filepath.Join("internal", "wailsui", "frontend", "favicon.png")
	f, err := os.Open(srcPath)
	must(err)
	defer f.Close()
	src, err := png.Decode(f)
	must(err)

	outDir := filepath.Join("tools", "msix", "Assets")
	must(os.MkdirAll(outDir, 0o755))

	for _, t := range targets {
		writePNG(filepath.Join(outDir, t.name), resize(src, t.w, t.h))
		fmt.Printf("生成 %-24s %d x %d\n", t.name, t.w, t.h)
	}

	// 宽磁贴：透明画布居中放 150x150 图标，两侧留白，避免把正方形图标拉伸变形。
	wide := image.NewNRGBA(image.Rect(0, 0, 310, 150))
	icon := resize(src, 150, 150)
	draw.Draw(wide, image.Rect(80, 0, 230, 150), icon, image.Point{}, draw.Over)
	writePNG(filepath.Join(outDir, "Wide310x150Logo.png"), wide)
	fmt.Printf("生成 %-24s %d x %d\n", "Wide310x150Logo.png", 310, 150)
}

// resize 用双线性插值把 src 缩放到 w x h，输出非预乘的 NRGBA。
func resize(src image.Image, w, h int) *image.NRGBA {
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	b := src.Bounds()
	sw, sh := float64(b.Dx()), float64(b.Dy())
	for y := 0; y < h; y++ {
		sy := (float64(y)+0.5)*sh/float64(h) - 0.5
		for x := 0; x < w; x++ {
			sx := (float64(x)+0.5)*sw/float64(w) - 0.5
			dst.SetNRGBA(x, y, bilinear(src, b, sx, sy))
		}
	}
	return dst
}

func bilinear(src image.Image, b image.Rectangle, x, y float64) color.NRGBA {
	x0, y0 := int(math.Floor(x)), int(math.Floor(y))
	fx, fy := x-float64(x0), y-float64(y0)
	pick := func(px, py int) color.NRGBA {
		if px < b.Min.X {
			px = b.Min.X
		} else if px > b.Max.X-1 {
			px = b.Max.X - 1
		}
		if py < b.Min.Y {
			py = b.Min.Y
		} else if py > b.Max.Y-1 {
			py = b.Max.Y - 1
		}
		return color.NRGBAModel.Convert(src.At(px, py)).(color.NRGBA)
	}
	a, bb := pick(x0, y0), pick(x0+1, y0)
	c, d := pick(x0, y0+1), pick(x0+1, y0+1)
	mix := func(p, q, r, s uint8) uint8 {
		top := float64(p)*(1-fx) + float64(q)*fx
		bottom := float64(r)*(1-fx) + float64(s)*fx
		return uint8(math.Round(top*(1-fy) + bottom*fy))
	}
	return color.NRGBA{
		R: mix(a.R, bb.R, c.R, d.R),
		G: mix(a.G, bb.G, c.G, d.G),
		B: mix(a.B, bb.B, c.B, d.B),
		A: mix(a.A, bb.A, c.A, d.A),
	}
}

func writePNG(path string, img image.Image) {
	f, err := os.Create(path)
	must(err)
	defer f.Close()
	must(png.Encode(f, img))
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
