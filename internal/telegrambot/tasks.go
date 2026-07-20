package telegrambot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// submitTask 校验授权和并发限制，创建任务后异步等待结果。
func (b *Bot) submitTask(ctx context.Context, message *Message, principal Principal, command, payload string, isCommand bool) {
	source, subscriptionURL, err := parseSubmission(command, payload, isCommand, message.Text)
	if err != nil {
		b.sendText(ctx, message.Chat.ID, err.Error())
		return
	}
	if principal.UserID == 0 {
		b.sendText(ctx, message.Chat.ID, "无法识别 Telegram 用户身份。")
		return
	}
	allowed, err := b.authorization.IsAuthorized(ctx, principal.UserID)
	if err != nil {
		b.config.Logger.Warn("telegram authorization failed", "user_id", principal.UserID, "chat_id", principal.ChatID, "error", err)
		b.sendText(ctx, message.Chat.ID, "权限校验失败，请稍后重试。")
		return
	}
	if !allowed {
		b.sendText(ctx, message.Chat.ID, "当前账号未获准提交测速任务。")
		return
	}

	// 在创建任务前占用槽位，避免任务已经进入服务端却没有 goroutine 等待其结果。
	select {
	case b.slots <- struct{}{}:
	case <-ctx.Done():
		return
	default:
		b.sendText(ctx, message.Chat.ID, "当前并发任务已满，请稍后再试。")
		return
	}
	if !b.reserveActive(principal.UserID) {
		<-b.slots
		b.sendText(ctx, message.Chat.ID, "你已有一个进行中的测速任务，请等待完成或使用 /cancel。")
		return
	}
	request := TaskRequest{Principal: principal, Source: source, SubscriptionURL: subscriptionURL}
	b.wg.Add(1)
	go b.submitAndReply(ctx, message.Chat.ID, principal.UserID, request)
}

// submitAndReply 在已经取得全局槽位和用户占位后执行可能较慢的订阅抓取、任务创建、
// 终态等待和图片发送。Run 可继续处理 /cancel 与 owner 权限命令。
func (b *Bot) submitAndReply(ctx context.Context, chatID, userID int64, request TaskRequest) {
	defer b.wg.Done()
	defer func() { <-b.slots }()
	activeTaskID := ""
	defer func() { b.clearActive(userID, activeTaskID) }()

	task, err := b.runner.Submit(ctx, request)
	if err != nil || strings.TrimSpace(task.ID) == "" {
		// Runner 错误可能包含订阅 URL 或代理原文，日志只记录具体类型。
		// 可向用户展示的内容必须通过 UserError 显式标记。
		b.config.Logger.Warn("telegram task submission failed", "user_id", userID, "chat_id", chatID, "error_type", fmt.Sprintf("%T", err))
		userMessage := UserMessage(err)
		if userMessage == "" {
			userMessage = "任务创建失败，请稍后重试或在管理页面查看日志。"
		}
		b.sendText(ctx, chatID, userMessage)
		return
	}
	activeTaskID = task.ID
	b.setActiveTaskID(userID, task.ID)
	// 确认消息只包含任务 ID，不回显可能含凭据的代理或订阅原文。
	b.sendText(ctx, chatID, fmt.Sprintf("测速任务已创建：%s", task.ID))
	b.waitAndReply(ctx, chatID, task.ID)
}

// waitAndReply 等待任务终态；成功、部分成功和全部失败都渲染图片，
// 主动取消等其他终态发送文本说明。
func (b *Bot) waitAndReply(ctx context.Context, chatID int64, taskID string) {
	completion, err := b.runner.Wait(ctx, taskID)
	if err != nil {
		if ctx.Err() == nil {
			b.config.Logger.Warn("telegram task wait failed", "task_id", taskID, "error", err)
			b.sendText(ctx, chatID, fmt.Sprintf("任务 %s 执行失败，请在管理页面查看详情。", taskID))
		}
		return
	}
	if completion.TaskID == "" {
		completion.TaskID = taskID
	}
	status := strings.ToLower(strings.TrimSpace(completion.Status))
	if status != "completed" && status != "partial" && status != "failed" {
		if status == "" {
			status = "unknown"
		}
		b.sendText(ctx, chatID, fmt.Sprintf("任务 %s 已结束，状态：%s。", taskID, status))
		return
	}
	image, err := b.renderer.Render(ctx, completion)
	if err != nil || len(image.Data) == 0 {
		b.config.Logger.Warn("telegram result rendering failed", "task_id", taskID, "error", err)
		b.sendText(ctx, chatID, fmt.Sprintf("任务 %s 已完成，但结果图片生成失败。", taskID))
		return
	}
	if strings.TrimSpace(image.Filename) == "" {
		image.Filename = "smalux-speedtest-" + taskID + ".png"
	}
	if strings.TrimSpace(image.Caption) == "" {
		image.Caption = fmt.Sprintf("Smalux Speedtest · 任务 %s · %s", taskID, status)
	}
	image.Caption = truncateRunes(image.Caption, 1024)
	// 图片上传不应继承可能持续十分钟的测速 Context；独立窗口让 Telegram 卡顿时
	// worker 和优雅关闭都有明确上限，同时不影响此前已完成的测速任务状态。
	deliveryCtx, deliveryCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	err = b.sendPhotoOrdered(deliveryCtx, chatID, image)
	deliveryCancel()
	if err != nil && ctx.Err() == nil {
		b.config.Logger.Warn("telegram sendPhoto failed", "task_id", taskID, "chat_id", chatID, "error", err)
		b.sendText(ctx, chatID, fmt.Sprintf("任务 %s 已完成，但结果图片发送失败。", taskID))
	}
}

// sendPhotoWithRetry 对网络错误、429 和 5xx 做有界重试。其他 4xx 通常是无效
// chat/文件等确定性错误，继续重试只会延迟失败回复。
func (b *Bot) sendPhotoWithRetry(ctx context.Context, chatID int64, image Image) error {
	var lastErr error
	for attempt := 1; attempt <= b.config.DeliveryAttempts; attempt++ {
		lastErr = b.api.SendPhoto(ctx, chatID, image.Filename, image.Caption, image.Data)
		if lastErr == nil {
			return nil
		}
		if ctx.Err() != nil || !retryableTelegramDelivery(lastErr) || attempt == b.config.DeliveryAttempts {
			return lastErr
		}
		if !waitContext(ctx, deliveryRetryDelay(lastErr, b.config.RetryDelay, 30*time.Second)) {
			return ctx.Err()
		}
	}
	return lastErr
}

func retryableTelegramDelivery(err error) bool {
	var apiError *APIError
	if !errors.As(err, &apiError) {
		return true
	}
	// Telegram 有时会用 HTTP 200 携带 ok=false；此时只能依赖 JSON error_code
	// 判断限流或服务端故障，不能只看 HTTP 状态码。
	if apiError.StatusCode == 429 || apiError.StatusCode >= 500 || apiError.ErrorCode == 429 || apiError.ErrorCode >= 500 {
		return true
	}
	// 没有任何协议级状态时通常是自定义 Transport 包装的临时错误；保留一次
	// 重试，但明确的 4xx/Telegram 4xx 业务错误不应重复发送。
	return apiError.StatusCode == 0 && apiError.ErrorCode == 0
}

// deliveryRetryDelay 优先采用 Telegram 的 retry_after，但始终限制最大等待时间，
// 避免恶意或异常网关返回极大整数让任务 goroutine 无限占用。
func deliveryRetryDelay(err error, fallback, maximum time.Duration) time.Duration {
	delay := fallback
	var apiError *APIError
	if errors.As(err, &apiError) && apiError.RetryAfterSeconds > 0 {
		seconds := int64(apiError.RetryAfterSeconds)
		if seconds <= int64((maximum / time.Second)) {
			delay = time.Duration(seconds) * time.Second
		} else {
			delay = maximum
		}
	}
	if delay < 0 {
		delay = 0
	}
	if maximum > 0 && delay > maximum {
		delay = maximum
	}
	return delay
}
