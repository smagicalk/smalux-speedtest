package reportpng

import (
	"bytes"
	"image"
	_ "image/png"
	"testing"
	"time"

	"smalux-speedtest/internal/model"
	"smalux-speedtest/internal/store"
)

// TestRenderProducesDecodablePNG 验证正常任务可以生成标准库可解码的 PNG，并确认固定
// 列宽与动态结果行高度符合公开资源边界。中文数据同时覆盖 rune 安全的文本路径；
// 在没有 CJK 字体的最小系统上允许回退字形，但图片编码不能失败。
func TestRenderProducesDecodablePNG(t *testing.T) {
	task := sampleTask()
	results := []model.SpeedResult{
		{
			TaskID: task.ID, ClientID: "client-a", ClientName: "上海客户端", ProxyID: "proxy-a", ProxyName: "东京节点",
			Protocol: "vless", MaskedAddress: "*.example.com:443", SpeedServerName: "Tokyo", Sponsor: "Example ISP",
			LatencyMS: 42.5, JitterMS: 3.2, DownloadBPS: 850_000_000, UploadBPS: 230_000_000,
		},
		{
			TaskID: task.ID, ClientID: "client-a", ClientName: "上海客户端", ProxyID: "proxy-a", ProxyName: "东京节点",
			Protocol: "vless", MaskedAddress: "*.example.com:443", SpeedServerName: "Osaka", Sponsor: "Example ISP",
			LatencyMS: 56.1, JitterMS: 4.8, DownloadBPS: 720_000_000, UploadBPS: 180_000_000,
		},
	}

	data, err := Render(task, results, len(results))
	if err != nil {
		t.Fatal(err)
	}
	configuration, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode PNG configuration: %v", err)
	}
	if format != "png" {
		t.Fatalf("format = %q, want png", format)
	}
	wantHeight := titleHeight + headerHeight + len(results)*rowHeight + footerHeight
	if configuration.Width != CanvasWidth || configuration.Height != wantHeight {
		t.Fatalf("dimensions = %dx%d, want %dx%d", configuration.Width, configuration.Height, CanvasWidth, wantHeight)
	}
	if _, _, err := image.Decode(bytes.NewReader(data)); err != nil {
		t.Fatalf("decode complete PNG: %v", err)
	}
}

// TestRenderEmptyResults 确认无结果任务仍生成带一行空状态的报告，而不是返回错误或
// 产生零高度 PNG。这样失败和刚创建的任务也能导出其参数与状态。
func TestRenderEmptyResults(t *testing.T) {
	data, err := Render(sampleTask(), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	configuration, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	wantHeight := titleHeight + headerHeight + rowHeight + footerHeight
	if format != "png" || configuration.Width != CanvasWidth || configuration.Height != wantHeight {
		t.Fatalf("empty report format/dimensions = %s %dx%d", format, configuration.Width, configuration.Height)
	}
}

// TestRenderCapsResultRows 传入超过 MaxRows 的结果，验证服务端无法被异常结果集诱导生成
// 无界画布；超出部分仅在页脚总数中体现。
func TestRenderCapsResultRows(t *testing.T) {
	results := make([]model.SpeedResult, MaxRows+20)
	for index := range results {
		results[index] = model.SpeedResult{
			ClientID: "client", ProxyID: "proxy", ProxyName: "node", Protocol: "socks",
			SpeedServerID: string(rune('a' + index%26)), SpeedServerName: "server", LatencyMS: float64(index + 1),
		}
	}
	data, err := Render(sampleTask(), results, len(results))
	if err != nil {
		t.Fatal(err)
	}
	configuration, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	wantHeight := titleHeight + headerHeight + MaxRows*rowHeight + footerHeight
	if configuration.Height != wantHeight {
		t.Fatalf("capped height = %d, want %d", configuration.Height, wantHeight)
	}
}

// TestVisibleResultsKeepsIdentityGroupsContiguous 验证同名代理或 Client 不会按延迟
// 相互穿插，否则表格左侧的纵向合并区会把同一身份拆成多个序号。
func TestVisibleResultsKeepsIdentityGroupsContiguous(t *testing.T) {
	results := []model.SpeedResult{
		{ProxyID: "proxy-b", ProxyName: "same", ClientID: "client", ClientName: "same", LatencyMS: 1},
		{ProxyID: "proxy-a", ProxyName: "same", ClientID: "client", ClientName: "same", LatencyMS: 20},
		{ProxyID: "proxy-b", ProxyName: "same", ClientID: "client", ClientName: "same", LatencyMS: 2},
		{ProxyID: "proxy-a", ProxyName: "same", ClientID: "client", ClientName: "same", LatencyMS: 10},
	}
	visible := visibleResults(results)
	for index, want := range []string{"proxy-a", "proxy-a", "proxy-b", "proxy-b"} {
		if visible[index].ProxyID != want {
			t.Fatalf("group order at %d = %q, want %q: %+v", index, visible[index].ProxyID, want, visible)
		}
	}
}

func sampleTask() store.Task {
	return store.Task{
		ID: "0123456789abcdef", Status: "completed", CandidateCount: 10, TopN: 3, Threads: 4,
		ProxyCount: 1, ClientCount: 1, CreatedAt: time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC).Format(time.RFC3339Nano),
	}
}
