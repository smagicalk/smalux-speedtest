package reportpng

import (
	"bytes"
	"fmt"
	"image"
	"image/png"

	"smalux-speedtest/internal/model"
	"smalux-speedtest/internal/store"
)

const (
	// CanvasWidth 是报告固定宽度，等于所有表格列宽之和。
	CanvasWidth = 1800
	// MaxRows 是单张报告最多绘制的结果条数。更多结果仍计入页脚总数，但不会继续
	// 扩大图片，调用方可同时提供 CSV 作为完整数据导出格式。
	MaxRows = 72

	titleHeight  = 92
	headerHeight = 58
	rowHeight    = 58
	footerHeight = 104
	maxTextRunes = 256
)

var columnWidths = [...]int{70, 260, 180, 120, 300, 130, 130, 190, 190, 230}

// Render 将 task 和已限制的 results 渲染为 PNG 字节。totalResults 是数据库中
// 完整结果数，可以大于 len(results)，用于在页脚明确标注截断。
//
// 结果先按代理、Client 和延迟稳定排序，再按“代理 ID + Client ID”合并左侧视觉分组。
// 最多绘制 MaxRows 条结果，保证输出尺寸不超过 CanvasWidth×4430 像素。空结果不是错误：
// Render 会生成带空状态行的有效报告，便于尚未完成或失败任务仍能导出任务元数据。
//
// Render 不修改 results，也不读取 ProxySpec 等含认证信息的数据结构。返回的错误仅来自
// 字体初始化或 PNG 编码。
func Render(task store.Task, results []model.SpeedResult, totalResults int) ([]byte, error) {
	faces, err := newReportFaces()
	if err != nil {
		return nil, fmt.Errorf("initialize report fonts: %w", err)
	}
	defer faces.Close()

	visible := visibleResults(results)
	if totalResults < len(results) {
		totalResults = len(results)
	}
	rows := len(visible)
	if rows == 0 {
		rows = 1
	}
	height := titleHeight + headerHeight + rows*rowHeight + footerHeight
	canvas := image.NewRGBA(image.Rect(0, 0, CanvasWidth, height))
	labels := labelsFor(faces.cjk)
	drawReport(canvas, faces, labels, task, visible, totalResults)

	var output bytes.Buffer
	if err := png.Encode(&output, canvas); err != nil {
		return nil, fmt.Errorf("encode report PNG: %w", err)
	}
	return output.Bytes(), nil
}

// reportLabels 根据当前字体是否覆盖中文选择界面文案。数据字段始终按 Unicode 绘制；
// 在最小系统上使用英文固定文案可确保报告结构仍完全可读。
type reportLabels struct {
	title      string
	headers    [10]string
	empty      string
	complete   string
	task       string
	clients    string
	proxies    string
	threads    string
	candidates string
	top        string
	results    string
	testTime   string
	disclaimer string
	showing    string
	of         string
}

func labelsFor(cjk bool) reportLabels {
	if cjk {
		return reportLabels{
			title:   "Smalux Speedtest · 分布式代理测速",
			headers: [10]string{"序号", "代理节点", "Client", "协议", "Speedtest 测速节点", "延迟", "抖动", "下载速度", "上传速度", "状态"},
			empty:   "暂无测速结果", complete: "完成", task: "任务", clients: "个 Client", proxies: "个代理",
			threads: "线程", candidates: "候选节点", top: "上下行节点", results: "结果", testTime: "测试时间",
			disclaimer: "测试结果仅供参考，以实际网络情况为准", showing: "显示", of: "共",
		}
	}
	return reportLabels{
		title:   "Smalux Speedtest - Distributed Proxy Benchmark",
		headers: [10]string{"No.", "Proxy", "Client", "Protocol", "Speedtest Server", "Latency", "Jitter", "Download", "Upload", "Status"},
		empty:   "No speed test results", complete: "Complete", task: "Task", clients: "clients", proxies: "proxies",
		threads: "Threads", candidates: "Candidates", top: "Transfer servers", results: "Results", testTime: "Test time",
		disclaimer: "Results are for reference and depend on actual network conditions", showing: "showing", of: "of",
	}
}
