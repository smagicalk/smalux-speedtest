package telegrambot

import (
	"context"
	"time"

	"smalux-speedtest/internal/logsafe"
)

const textDeliveryTimeout = 8 * time.Second

// outboundDelivery 是单个会话队列中的一次 Telegram API 调用。
// send 只会由该 Chat ID 对应的 worker 调用，因此同一会话内不会并发发送；done 使用
// 缓冲通道，允许等待方因超时离开后 worker 仍能完成清理而不会被反向阻塞。
type outboundDelivery struct {
	send    func() error
	done    chan<- error
	release func()
}

// enqueueDelivery 把一次出站调用追加到对应会话的 FIFO 队列。每个会话至多有一个
// worker，所以同一 chat 的“任务已创建”、结果图片和失败提示保持调用时的顺序；不同
// chat 使用独立 worker，某个用户的 Telegram 请求变慢不会阻塞其他用户或 getUpdates。
func (b *Bot) enqueueDelivery(chatID int64, delivery outboundDelivery) {
	b.deliveryWG.Add(1)
	b.deliveryMu.Lock()
	_, workerRunning := b.deliveryQueues[chatID]
	b.deliveryQueues[chatID] = append(b.deliveryQueues[chatID], delivery)
	b.deliveryMu.Unlock()
	if !workerRunning {
		go b.drainDeliveryQueue(chatID)
	}
}

// drainDeliveryQueue 顺序排空一个会话。空队列在锁内删除，避免 worker 即将退出时新消息
// 被追加到一个已经无人消费的切片：若入队发生在删除之后，它会看到不存在的键并启动
// 新 worker；若发生在删除之前，当前 worker 下一轮会继续消费。
func (b *Bot) drainDeliveryQueue(chatID int64) {
	for {
		b.deliveryMu.Lock()
		queue, ok := b.deliveryQueues[chatID]
		if !ok || len(queue) == 0 {
			delete(b.deliveryQueues, chatID)
			b.deliveryMu.Unlock()
			return
		}
		delivery := queue[0]
		queue[0] = outboundDelivery{}
		b.deliveryQueues[chatID] = queue[1:]
		b.deliveryMu.Unlock()

		err := delivery.send()
		if delivery.done != nil {
			delivery.done <- err
		}
		if delivery.release != nil {
			delivery.release()
		}
		b.deliveryWG.Done()
	}
}

// sendText 仅执行常量时间的入队操作，避免慢 Telegram 出站请求阻塞 getUpdates。
// textSlots 全局限制最多 32 条待发送或发送中的文本，防止 API 故障期间消息洪峰无限占用
// 内存；结果图片不占文本槽位，测速完成通知不会被普通命令回复挤掉。
func (b *Bot) sendText(ctx context.Context, chatID int64, text string) {
	b.sendTextRequest(ctx, MessageRequest{ChatID: chatID, Text: text})
}

// sendTextRequest 异步发送普通提示或带按钮的菜单。
func (b *Bot) sendTextRequest(ctx context.Context, request MessageRequest) {
	select {
	case b.textSlots <- struct{}{}:
	default:
		b.config.Logger.Warn("telegram text delivery queue full", "chat_id", request.ChatID)
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	baseContext := context.WithoutCancel(ctx)
	request.Text = truncateRunes(request.Text, 4096)
	b.enqueueDelivery(request.ChatID, outboundDelivery{
		send: func() error {
			deliveryCtx, cancel := context.WithTimeout(baseContext, textDeliveryTimeout)
			defer cancel()
			_, err := b.sendTextWithRetry(deliveryCtx, request)
			if err != nil && deliveryCtx.Err() == nil {
				b.config.Logger.Warn("telegram sendMessage failed", "chat_id", request.ChatID, "error_type", logsafe.ErrorType(err))
			}
			return err
		},
		release: func() { <-b.textSlots },
	})
}

// sendTextWithRetry 对短文本做一次有界重试。长的 RetryDelay 会压低为 2 秒，避免
// 高频交互反馈因为一条临时失败占住同一会话队列过久。
func (b *Bot) sendTextWithRetry(ctx context.Context, request MessageRequest) (SentMessage, error) {
	var sent SentMessage
	var err error
	for attempt := 1; attempt <= 2; attempt++ {
		sent, err = b.api.SendMessage(ctx, request)
		if err == nil || ctx.Err() != nil || !retryableTelegramDelivery(err) || attempt == 2 {
			return sent, err
		}
		// 文本反馈要保持交互响应；即使 Telegram 给出更长 retry_after，也只
		// 在短窗口内再次尝试，避免同一会话队列被单条提示长时间占住。
		wait := deliveryRetryDelay(err, b.config.RetryDelay, 2*time.Second)
		if !waitContext(ctx, wait) {
			return SentMessage{}, ctx.Err()
		}
	}
	return sent, err
}

// sendMessageOrdered 等待消息实际发送完成并返回其 ID，供任务进度后续原地编辑。
func (b *Bot) sendMessageOrdered(ctx context.Context, request MessageRequest) (SentMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	request.Text = truncateRunes(request.Text, 4096)
	done := make(chan error, 1)
	var sent SentMessage
	b.enqueueDelivery(request.ChatID, outboundDelivery{
		send: func() error {
			var err error
			sent, err = b.sendTextWithRetry(ctx, request)
			return err
		},
		done: done,
	})
	select {
	case err := <-done:
		return sent, err
	case <-ctx.Done():
		return SentMessage{}, ctx.Err()
	}
}

func (b *Bot) editMessageOrdered(ctx context.Context, request EditMessageRequest) error {
	if ctx == nil {
		ctx = context.Background()
	}
	request.Text = truncateRunes(request.Text, 4096)
	done := make(chan error, 1)
	b.enqueueDelivery(request.ChatID, outboundDelivery{
		send: func() error { return b.api.EditMessageText(ctx, request) },
		done: done,
	})
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *Bot) deleteMessageOrdered(ctx context.Context, chatID, messageID int64) error {
	if messageID == 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	done := make(chan error, 1)
	b.enqueueDelivery(chatID, outboundDelivery{
		send: func() error { return b.api.DeleteMessage(ctx, chatID, messageID) },
		done: done,
	})
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// sendPhotoOrdered 把结果图片放入和文本相同的会话队列，并把最终发送结果返回给任务
// goroutine。ctx 控制从入队到完成的总时间；即使等待方超时，队列 worker 仍会取出这个
// 已过期调用并完成 WaitGroup/队列清理，后续失败提示也会排在它之后。
func (b *Bot) sendPhotoOrdered(ctx context.Context, chatID int64, image Image) error {
	if ctx == nil {
		ctx = context.Background()
	}
	done := make(chan error, 1)
	b.enqueueDelivery(chatID, outboundDelivery{
		send: func() error {
			return b.sendPhotoWithRetry(ctx, chatID, image)
		},
		done: done,
	})
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
