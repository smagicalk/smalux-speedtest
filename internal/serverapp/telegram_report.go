package serverapp

import (
	"context"
	"errors"
	"fmt"

	"smalux-speedtest/internal/reportpng"
	"smalux-speedtest/internal/telegrambot"
)

// telegramReportSlots 把高峰 RGBA 画布限制为一张。Bot 仍可并发等待和上传，
// 但不会因多个任务同时终结而并发分配多个约 32 MiB 的最大画布。
var telegramReportSlots = make(chan struct{}, 1)

// telegramReportRenderer 将 Runner.Wait 生成的脱敏结果快照转换为 Bot 上传对象。
// reportpng 只接收持久化 Task 和 SpeedResult，不可能访问 Hub 内含凭据的 Assignment。
type telegramReportRenderer struct{}

func (telegramReportRenderer) Render(ctx context.Context, completion telegrambot.Completion) (telegrambot.Image, error) {
	if err := ctx.Err(); err != nil {
		return telegrambot.Image{}, err
	}
	payload, ok := completion.Payload.(telegramTaskPayload)
	if !ok {
		return telegrambot.Image{}, errors.New("telegram completion payload has an unexpected type")
	}
	select {
	case telegramReportSlots <- struct{}{}:
		defer func() { <-telegramReportSlots }()
	case <-ctx.Done():
		return telegrambot.Image{}, ctx.Err()
	}
	data, err := reportpng.Render(payload.Task, payload.Results, payload.TotalResults)
	if err != nil {
		return telegrambot.Image{}, err
	}
	if err := ctx.Err(); err != nil {
		return telegrambot.Image{}, err
	}
	shortID := completion.TaskID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}
	return telegrambot.Image{
		Filename: fmt.Sprintf("smalux-speedtest-%s.png", shortID),
		Caption: fmt.Sprintf("Smalux Speedtest · 任务 %s · %s · %d 条结果",
			shortID, completion.Status, payload.TotalResults),
		Data: data,
	}, nil
}

var _ telegrambot.Renderer = telegramReportRenderer{}
