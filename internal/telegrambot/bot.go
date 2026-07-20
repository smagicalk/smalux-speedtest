package telegrambot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"smalux-speedtest/internal/logsafe"
)

const (
	// StartText 是 /start 的简短欢迎消息。
	StartText = "Smalux Speedtest Bot\n测速前需要 owner 授权。使用 /id 获取授权所需 ID。\n授权后可直接发送代理，或使用 /sub <URL> 提交订阅。使用 /help 查看完整用法。"
	// HelpText 说明文本解析规则，尤其说明 HTTP 代理与订阅 URL 的歧义处理方式。
	HelpText = "测速用法：\n1. 直接发送代理链接或多行代理列表，所有普通文本均按代理源处理。\n2. 使用 /test <代理文本> 明确提交代理。\n3. 只有 /sub <HTTP(S) URL> 会按订阅地址处理。\n4. /cancel 取消自己当前的任务，/id 查看 Telegram ID。\nOwner 命令：/authorize <user_id>、/revoke <user_id>、/users；授权和撤销也支持回复目标消息。\n任务完成后 Bot 会返回结果图片。"
)

// Config 控制 Bot 的轮询、重试和并发行为。
type Config struct {
	// PollTimeout 是单次 getUpdates 长轮询时长；默认 25 秒，最大限制为 50 秒。
	PollTimeout time.Duration
	// RetryDelay 是 Telegram API 临时错误后的等待时间；默认 2 秒。
	RetryDelay time.Duration
	// MaxConcurrentTasks 是同时等待并渲染的任务数；默认 4。
	MaxConcurrentTasks int
	// DeliveryAttempts 是结果图片遇到临时 Telegram 错误时的最多发送次数；默认 3。
	DeliveryAttempts int
	// UpdateOffsetStore 可选持久化 getUpdates offset；生产服务应提供，单元测试可留空。
	UpdateOffsetStore UpdateOffsetStore
	// Logger 接收轮询、任务和发送错误；为 nil 时使用 slog.Default。
	Logger *slog.Logger
}

// Bot 负责 Telegram 更新循环和测速任务异步编排。
// 同一个 Bot 实例同一时间只能调用一次 Run。
type Bot struct {
	api           API
	authorization AuthorizationManager
	runner        Runner
	renderer      Renderer
	config        Config
	slots         chan struct{}
	wg            sync.WaitGroup
	activeMu      sync.Mutex
	// active 按 Telegram User ID 保存任务 ID 和取消状态；空任务 ID 表示已经预留、
	// 正在创建任务。全部字段都只在 activeMu 下访问。
	active map[int64]activeTaskState
	// offsetStore 防止服务重启后重放已处理的提交更新。
	offsetStore UpdateOffsetStore
	// textSlots 限制待发送和发送中的反馈文本数量；文本回复不能阻塞 getUpdates 主循环，
	// 但也不能在 Telegram 故障时无限积压内存。
	textSlots chan struct{}
	// deliveryMu 和 deliveryQueues 构成按 Chat ID 隔离的 FIFO 出站队列。
	// 同一会话的文本和结果图片严格按入队顺序发送，不同会话由各自 worker 并行处理。
	deliveryMu     sync.Mutex
	deliveryQueues map[int64][]outboundDelivery
	// deliveryWG 让 Run 退出前等待所有已经入队的文本和图片发送完成。
	deliveryWG sync.WaitGroup
}

// New 创建 Bot，并要求四个外部端口全部显式提供。
// 强制注入 AuthorizationManager 可避免配置遗漏时意外开放高带宽任务或管理命令。
func New(api API, authorization AuthorizationManager, runner Runner, renderer Renderer, config Config) (*Bot, error) {
	if api == nil {
		return nil, errors.New("telegram API is required")
	}
	if authorization == nil {
		return nil, errors.New("telegram authorization manager is required")
	}
	if runner == nil {
		return nil, errors.New("telegram task runner is required")
	}
	if renderer == nil {
		return nil, errors.New("telegram result renderer is required")
	}
	if config.PollTimeout <= 0 {
		config.PollTimeout = 25 * time.Second
	}
	if config.PollTimeout > 50*time.Second {
		config.PollTimeout = 50 * time.Second
	}
	if config.RetryDelay <= 0 {
		config.RetryDelay = 2 * time.Second
	}
	if config.MaxConcurrentTasks <= 0 {
		config.MaxConcurrentTasks = 4
	}
	if config.DeliveryAttempts <= 0 {
		config.DeliveryAttempts = 3
	} else if config.DeliveryAttempts > 5 {
		config.DeliveryAttempts = 5
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	return &Bot{
		api: api, authorization: authorization, runner: runner, renderer: renderer, config: config,
		slots:          make(chan struct{}, config.MaxConcurrentTasks),
		active:         make(map[int64]activeTaskState),
		offsetStore:    config.UpdateOffsetStore,
		textSlots:      make(chan struct{}, 32),
		deliveryQueues: make(map[int64][]outboundDelivery),
	}, nil
}

// Run 使用 getUpdates 长轮询处理消息，直到 ctx 取消。
//
// offset 在处理每批更新前按最大 UpdateID 推进，包括无法识别的更新，防止坏消息被无限
// 重放。Telegram 临时错误会记录并重试，不会终止 Bot。退出前等待已启动的任务 goroutine；
// Runner.Wait 和 Renderer.Render 必须遵守传入的 Context，否则优雅关闭也会被其阻塞。
func (b *Bot) Run(ctx context.Context) error {
	offset := int64(0)
	if b.offsetStore != nil {
		var err error
		offset, err = b.offsetStore.LoadTelegramUpdateOffset(ctx)
		if err != nil {
			return fmt.Errorf("load telegram update offset: %w", err)
		}
	}
	for {
		updates, err := b.api.GetUpdates(ctx, offset, b.config.PollTimeout)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			b.config.Logger.Warn("telegram getUpdates failed", "error_type", logsafe.ErrorType(err))
			if !waitContext(ctx, b.config.RetryDelay) {
				break
			}
			continue
		}
		persistFailed := false
		for _, update := range updates {
			if update.UpdateID < offset {
				continue
			}
			nextOffset := update.UpdateID + 1
			if b.offsetStore != nil {
				if err := b.offsetStore.SaveTelegramUpdateOffset(ctx, nextOffset); err != nil {
					if ctx.Err() != nil {
						persistFailed = true
						break
					}
					b.config.Logger.Warn("persist telegram update offset failed", "offset", nextOffset, "error_type", logsafe.ErrorType(err))
					persistFailed = true
					break
				}
			}
			offset = nextOffset
			b.handleUpdate(ctx, update)
		}
		if persistFailed && !waitContext(ctx, b.config.RetryDelay) {
			break
		}
		if ctx.Err() != nil {
			break
		}
	}
	b.wg.Wait()
	// 先等待任务 goroutine，确保不会再有结果图片或完成提示入队；再排空出站队列。
	// 这个顺序也满足 sync.WaitGroup 的约束：Wait 开始后不会再并发 Add。
	b.deliveryWG.Wait()
	return ctx.Err()
}

// handleUpdate 处理单条私聊文本消息。群组、频道和非文本更新只推进 offset，不回复。
func (b *Bot) handleUpdate(ctx context.Context, update Update) {
	message := update.Message
	if message == nil || message.Chat.ID == 0 || message.Chat.Type != "private" || strings.TrimSpace(message.Text) == "" {
		return
	}
	principal := principalFromMessage(message)
	command, payload, isCommand := splitCommand(message.Text)
	switch command {
	case "start":
		b.sendText(ctx, message.Chat.ID, StartText)
		return
	case "help":
		b.sendText(ctx, message.Chat.ID, HelpText)
		return
	case "id":
		b.sendText(ctx, message.Chat.ID, fmt.Sprintf("User ID：%d\nChat ID：%d", principal.UserID, principal.ChatID))
		return
	case "authorize", "revoke", "users":
		b.handleAuthorizationCommand(ctx, message, principal, command, payload)
		return
	case "cancel":
		b.handleCancel(ctx, message.Chat.ID, principal)
		return
	}
	b.submitTask(ctx, message, principal, command, payload, isCommand)
}
