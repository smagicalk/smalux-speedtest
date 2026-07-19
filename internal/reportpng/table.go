package reportpng

import (
	"fmt"
	"image"
	"image/color"
	"strings"

	"smalux-speedtest/internal/model"
)

func drawResultRows(canvas *image.RGBA, faces *reportFaces, labels reportLabels, results []model.SpeedResult, offsets [10]int, dataTop int) {
	for row, result := range results {
		y := dataTop + row*rowHeight
		server := limitRunes(result.SpeedServerName, maxTextRunes)
		if server == "" {
			server = "-"
		}
		if result.Sponsor != "" {
			server += " · " + limitRunes(result.Sponsor, maxTextRunes)
		}
		status := labels.complete
		textColor := color.RGBA{R: 23, G: 33, B: 38, A: 255}
		if result.Error != "" {
			status = result.Error
			textColor = color.RGBA{R: 168, G: 36, B: 36, A: 255}
		}
		cells := [10]string{"", "", "", "", server, formatDelay(result.LatencyMS), formatDelay(result.JitterMS), formatSpeed(result.DownloadBPS), formatSpeed(result.UploadBPS), status}
		for column, value := range cells {
			drawText(canvas, faces.body, value, image.Rect(offsets[column]+6, y, offsets[column]+columnWidths[column]-6, y+rowHeight), textColor, alignCenter, false)
		}
		horizontalLine(canvas, y+rowHeight-1, offsets[4], CanvasWidth, color.RGBA{R: 220, G: 226, B: 229, A: 255})
	}
}

// resultGroup 描述左侧四列的纵向合并区域。
type resultGroup struct {
	start int
	count int
	item  model.SpeedResult
}

func drawGroups(canvas *image.RGBA, faces *reportFaces, results []model.SpeedResult, offsets [10]int, dataTop int) {
	groups := make([]resultGroup, 0, len(results))
	lastKey := ""
	for index, result := range results {
		key := result.ProxyID + "\x00" + result.ClientID
		if len(groups) > 0 && key == lastKey {
			groups[len(groups)-1].count++
			continue
		}
		groups = append(groups, resultGroup{start: index, count: 1, item: result})
		lastKey = key
	}
	for index, group := range groups {
		y := dataTop + group.start*rowHeight
		bottom := y + group.count*rowHeight
		client := group.item.ClientName
		if client == "" {
			client = shortID(group.item.ClientID)
		}
		values := [4]string{fmt.Sprintf("%d", index+1), group.item.ProxyName, client, strings.ToUpper(limitRunes(group.item.Protocol, 64))}
		for column, value := range values {
			drawText(canvas, faces.body, value, image.Rect(offsets[column]+6, y, offsets[column]+columnWidths[column]-6, bottom), color.RGBA{R: 23, G: 33, B: 38, A: 255}, alignCenter, column == 1)
		}
		horizontalLine(canvas, bottom-1, 0, offsets[4], color.RGBA{R: 207, G: 215, B: 218, A: 255})
	}
}

// columnOffsets 返回每列左边界；最后一个右边界由 CanvasWidth 表示。
func columnOffsets() [10]int {
	var offsets [10]int
	for index := 1; index < len(offsets); index++ {
		offsets[index] = offsets[index-1] + columnWidths[index-1]
	}
	return offsets
}
