package serverapp

import (
	"bytes"
	"image/png"
	"strings"
	"testing"
	"time"

	"smalux-speedtest/internal/model"
	"smalux-speedtest/internal/store"
	"smalux-speedtest/internal/telegrambot"
)

// TestTelegramReportRendererProducesDecodablePNG 验证服务端 Renderer 能把 Wait 的内部
// payload 转换为 Telegram 上传元数据和标准 PNG，而不依赖浏览器、网络或图形系统。
func TestTelegramReportRendererProducesDecodablePNG(t *testing.T) {
	const taskID = "0123456789abcdef"
	task := store.Task{
		ID: taskID, Status: "completed", CandidateCount: 10, TopN: 3, Threads: 4,
		ProxyCount: 1, ClientCount: 1, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	results := []model.SpeedResult{{
		TaskID: taskID, ClientID: "client-1", ClientName: "client", ProxyID: "proxy-1", ProxyName: "node",
		Protocol: "vless", MaskedAddress: "*.example.com:443", SpeedServerID: "server-1", SpeedServerName: "Tokyo",
		LatencyMS: 32.5, JitterMS: 2.1, DownloadBPS: 900_000_000, UploadBPS: 250_000_000,
	}}
	completion := telegrambot.Completion{
		TaskID: taskID, Status: "completed",
		Payload: telegramTaskPayload{Task: task, Results: results, TotalResults: len(results)},
	}
	image, err := (telegramReportRenderer{}).Render(t.Context(), completion)
	if err != nil {
		t.Fatal(err)
	}
	if image.Filename != "smalux-speedtest-01234567.png" {
		t.Fatalf("filename = %q", image.Filename)
	}
	if !strings.Contains(image.Caption, "01234567") || !strings.Contains(image.Caption, "completed") || !strings.Contains(image.Caption, "1 条结果") {
		t.Fatalf("unexpected caption: %q", image.Caption)
	}
	decoded, err := png.Decode(bytes.NewReader(image.Data))
	if err != nil {
		t.Fatalf("decode rendered PNG: %v", err)
	}
	if decoded.Bounds().Dx() <= 0 || decoded.Bounds().Dy() <= 0 {
		t.Fatalf("rendered PNG has invalid bounds: %v", decoded.Bounds())
	}
}
