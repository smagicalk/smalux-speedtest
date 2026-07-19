package reportpng

import (
	"image"
	"image/color"
	"image/draw"
	"math"

	"golang.org/x/image/font"
	"golang.org/x/image/math/fixed"
)

func drawHeatCell(canvas *image.RGBA, rectangle image.Rectangle, normalized float64, accent color.RGBA, bar bool) {
	alpha := 0.15 + normalized*0.58
	background := blend(color.RGBA{R: 255, G: 255, B: 255, A: 255}, accent, alpha)
	fill(canvas, rectangle, background)
	if bar && normalized > 0 {
		barWidth := int(float64(rectangle.Dx()-16) * normalized)
		fill(canvas, image.Rect(rectangle.Min.X+8, rectangle.Max.Y-8, rectangle.Min.X+8+barWidth, rectangle.Max.Y-4), accent)
	}
}

func blend(base, accent color.RGBA, amount float64) color.RGBA {
	if amount < 0 {
		amount = 0
	} else if amount > 1 {
		amount = 1
	}
	mix := func(a, b uint8) uint8 { return uint8(math.Round(float64(a)*(1-amount) + float64(b)*amount)) }
	return color.RGBA{R: mix(base.R, accent.R), G: mix(base.G, accent.G), B: mix(base.B, accent.B), A: 255}
}

type textAlignment uint8

const (
	alignLeft textAlignment = iota
	alignCenter
)

// drawText 在 rectangle 内垂直居中并裁剪文本。bold 通过偏移一个像素重复绘制实现，
// 无需假设系统 CJK 字体同时安装了独立粗体文件。
func drawText(canvas *image.RGBA, face font.Face, value string, rectangle image.Rectangle, textColor color.Color, alignment textAlignment, bold bool) {
	rectangle = rectangle.Intersect(canvas.Bounds())
	if rectangle.Empty() {
		return
	}
	clipped := canvas.SubImage(rectangle).(*image.RGBA)
	drawer := &font.Drawer{Dst: clipped, Src: image.NewUniform(textColor), Face: face}
	value = fitText(drawer, value, rectangle.Dx())
	width := drawer.MeasureString(value).Ceil()
	x := rectangle.Min.X
	if alignment == alignCenter {
		x += (rectangle.Dx() - width) / 2
	}
	metrics := face.Metrics()
	textHeight := (metrics.Ascent + metrics.Descent).Ceil()
	baseline := rectangle.Min.Y + (rectangle.Dy()-textHeight)/2 + metrics.Ascent.Ceil()
	drawer.Dot = fixed.P(x, baseline)
	drawer.DrawString(value)
	if bold {
		drawer.Dot = fixed.P(x+1, baseline)
		drawer.DrawString(value)
	}
}

// fitText 最多检查 maxTextRunes 个 rune，并用二分搜索找出适合单元格的最长前缀。
// 这样超长、不可信节点名不会导致二次复杂度测量或越过相邻列。
func fitText(drawer *font.Drawer, value string, maxWidth int) string {
	runes, limited := collectRunes(value, maxTextRunes)
	if !limited && drawer.MeasureString(string(runes)).Ceil() <= maxWidth {
		return string(runes)
	}
	const suffix = "..."
	if drawer.MeasureString(suffix).Ceil() > maxWidth {
		return ""
	}
	low, high := 0, len(runes)
	for low < high {
		middle := (low + high + 1) / 2
		if drawer.MeasureString(string(runes[:middle])+suffix).Ceil() <= maxWidth {
			low = middle
		} else {
			high = middle - 1
		}
	}
	return string(runes[:low]) + suffix
}

func fill(destination draw.Image, rectangle image.Rectangle, fillColor color.Color) {
	draw.Draw(destination, rectangle, image.NewUniform(fillColor), image.Point{}, draw.Src)
}

func horizontalLine(destination draw.Image, y, left, right int, lineColor color.Color) {
	fill(destination, image.Rect(left, y, right, y+1), lineColor)
}

func verticalLine(destination draw.Image, x, top, bottom int, lineColor color.Color) {
	fill(destination, image.Rect(x, top, x+1, bottom), lineColor)
}
