package reportpng

import (
	"fmt"
	"image"
	"image/color"

	"smalux-speedtest/internal/model"
	"smalux-speedtest/internal/store"
)

// drawReport 按背景、热力格、水印、文字、网格和页脚的顺序绘制，避免后绘制的不透明
// 背景遮住水印或文字。
func drawReport(canvas *image.RGBA, faces *reportFaces, labels reportLabels, task store.Task, results []model.SpeedResult, totalResults int) {
	width := canvas.Bounds().Dx()
	height := canvas.Bounds().Dy()
	fill(canvas, canvas.Bounds(), color.RGBA{R: 247, G: 248, B: 249, A: 255})

	drawText(canvas, faces.title, labels.title, image.Rect(0, 6, width, 62), color.RGBA{R: 23, G: 33, B: 38, A: 255}, alignCenter, true)
	subtitle := fmt.Sprintf("%s %s · %d %s · %d %s", labels.task, shortID(task.ID), task.ClientCount, labels.clients, task.ProxyCount, labels.proxies)
	drawText(canvas, faces.subtitle, subtitle, image.Rect(0, 48, width, 88), color.RGBA{R: 102, G: 115, B: 122, A: 255}, alignCenter, false)

	headerRect := image.Rect(0, titleHeight, width, titleHeight+headerHeight)
	fill(canvas, headerRect, color.RGBA{R: 232, G: 236, B: 238, A: 255})
	offsets := columnOffsets()
	for index, header := range labels.headers {
		drawText(canvas, faces.header, header, image.Rect(offsets[index], titleHeight, offsets[index]+columnWidths[index], titleHeight+headerHeight), color.RGBA{R: 41, G: 52, B: 58, A: 255}, alignCenter, true)
	}

	dataRows := len(results)
	if dataRows == 0 {
		dataRows = 1
	}
	dataTop := titleHeight + headerHeight
	dataBottom := dataTop + dataRows*rowHeight
	maxLatency := maxMetric(results, func(result model.SpeedResult) float64 { return result.LatencyMS })
	maxJitter := maxMetric(results, func(result model.SpeedResult) float64 { return result.JitterMS })
	maxDownload := maxMetric(results, func(result model.SpeedResult) float64 { return result.DownloadBPS })
	maxUpload := maxMetric(results, func(result model.SpeedResult) float64 { return result.UploadBPS })

	// 第一遍只画行背景和热力色，给水印保留统一的底层。
	for row := 0; row < dataRows; row++ {
		y := dataTop + row*rowHeight
		base := color.RGBA{R: 255, G: 255, B: 255, A: 255}
		if row%2 == 1 {
			base = color.RGBA{R: 251, G: 252, B: 252, A: 255}
		}
		fill(canvas, image.Rect(0, y, width, y+rowHeight), base)
		if len(results) == 0 {
			continue
		}
		result := results[row]
		drawHeatCell(canvas, image.Rect(offsets[5], y, offsets[5]+columnWidths[5], y+rowHeight), ratio(result.LatencyMS, maxLatency), color.RGBA{R: 114, G: 210, B: 220, A: 255}, false)
		drawHeatCell(canvas, image.Rect(offsets[6], y, offsets[6]+columnWidths[6], y+rowHeight), ratio(result.JitterMS, maxJitter), color.RGBA{R: 114, G: 210, B: 220, A: 255}, false)
		drawHeatCell(canvas, image.Rect(offsets[7], y, offsets[7]+columnWidths[7], y+rowHeight), ratio(result.DownloadBPS, maxDownload), color.RGBA{R: 255, G: 63, B: 125, A: 255}, true)
		drawHeatCell(canvas, image.Rect(offsets[8], y, offsets[8]+columnWidths[8], y+rowHeight), ratio(result.UploadBPS, maxUpload), color.RGBA{R: 255, G: 63, B: 125, A: 255}, true)
	}

	// 水印使用低对比度固定文本；即使字体不覆盖中文也始终可见。
	drawText(canvas, faces.title, "SMALUX SPEEDTEST", image.Rect(0, dataTop, width, dataBottom), color.RGBA{R: 220, G: 235, B: 230, A: 255}, alignCenter, true)

	if len(results) == 0 {
		drawText(canvas, faces.body, labels.empty, image.Rect(0, dataTop, width, dataBottom), color.RGBA{R: 102, G: 115, B: 122, A: 255}, alignCenter, false)
	} else {
		drawResultRows(canvas, faces, labels, results, offsets, dataTop)
		drawGroups(canvas, faces, results, offsets, dataTop)
	}

	// 纵向网格统一绘制，避免逐单元格描边导致相邻边颜色变深。
	for _, x := range offsets[1:] {
		verticalLine(canvas, x, titleHeight, dataBottom, color.RGBA{R: 212, G: 218, B: 222, A: 255})
	}
	verticalLine(canvas, width-1, titleHeight, dataBottom, color.RGBA{R: 212, G: 218, B: 222, A: 255})
	horizontalLine(canvas, titleHeight, 0, width, color.RGBA{R: 207, G: 215, B: 218, A: 255})
	horizontalLine(canvas, dataBottom-1, 0, width, color.RGBA{R: 207, G: 215, B: 218, A: 255})

	footerY := height - footerHeight
	fill(canvas, image.Rect(0, footerY, width, height), color.RGBA{R: 232, G: 236, B: 238, A: 255})
	countText := fmt.Sprintf("%s=%d   %s=%d   %s=Top %d   %s=%d", labels.threads, task.Threads, labels.candidates, task.CandidateCount, labels.top, task.TopN, labels.results, totalResults)
	if totalResults > len(results) {
		countText += fmt.Sprintf(" (%s %d %s %d)", labels.showing, len(results), labels.of, totalResults)
	}
	drawText(canvas, faces.subtitle, countText, image.Rect(22, footerY+10, width-22, footerY+54), color.RGBA{R: 41, G: 52, B: 58, A: 255}, alignLeft, false)
	timeText := fmt.Sprintf("%s: %s   %s", labels.testTime, formatTaskTime(task.CreatedAt), labels.disclaimer)
	drawText(canvas, faces.small, timeText, image.Rect(22, footerY+52, width-22, footerY+96), color.RGBA{R: 102, G: 115, B: 122, A: 255}, alignLeft, false)
}
