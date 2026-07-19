package serverapp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"smalux-speedtest/internal/telegrambot"
)

// telegramRuntime 保存 Bot 后台循环的取消与完成信号，确保关闭 SQLite 前所有 Bot
// 任务都已停止访问任务和授权数据。
type telegramRuntime struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// startTelegram 校验成对配置、同步唯一 owner，并启动 Telegram getUpdates 长轮询。
// Token 只交给 HTTPAPI，任何成功日志都不会包含 Token 或代理提交内容。
func (a *App) startTelegram(parent context.Context) error {
	if parent == nil {
		parent = context.Background()
	}
	token := strings.TrimSpace(a.config.TelegramBotToken)
	ownerID := a.config.TelegramOwnerID
	if token == "" && ownerID == 0 {
		return nil
	}
	if token == "" {
		return errors.New("telegram bot token is required when telegram owner ID is configured")
	}
	if ownerID <= 0 {
		return errors.New("telegram owner ID must be positive when telegram bot is enabled")
	}
	botID, err := telegramBotIDFromToken(token)
	if err != nil {
		return fmt.Errorf("initialize telegram update offset: %w", err)
	}
	api, err := telegrambot.NewHTTPAPI(telegrambot.HTTPConfig{
		Token: token, BaseURL: a.config.TelegramAPIBaseURL,
	})
	if err != nil {
		return fmt.Errorf("initialize telegram API: %w", err)
	}
	if _, err := a.store.SyncTelegramOwner(parent, ownerID, "", ""); err != nil {
		return fmt.Errorf("synchronize telegram owner: %w", err)
	}
	bot, err := telegrambot.New(
		api,
		telegramAuthorization{store: a.store},
		telegramTaskRunner{app: a},
		telegramReportRenderer{},
		telegrambot.Config{Logger: a.config.Logger, UpdateOffsetStore: telegramOffsetStore{app: a, botID: botID}},
	)
	if err != nil {
		return fmt.Errorf("initialize telegram bot: %w", err)
	}
	ctx, cancel := context.WithCancel(parent)
	runtime := &telegramRuntime{cancel: cancel, done: make(chan struct{})}
	a.telegram = runtime
	go func() {
		defer close(runtime.done)
		if err := bot.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			a.config.Logger.Error("telegram bot stopped", "error", err)
		}
	}()
	a.config.Logger.Info("telegram bot enabled", "owner_user_id", ownerID)
	return nil
}

// cancelTelegram 只发出停止信号，使 HTTP Shutdown 可以与 Bot 清理并行等待。
func (a *App) cancelTelegram() {
	if a.telegram != nil {
		a.telegram.cancel()
	}
}

// waitTelegram 等待长轮询和所有结果 goroutine 退出，并受 Shutdown 截止时间约束。
func (a *App) waitTelegram(ctx context.Context) error {
	if a.telegram == nil {
		return nil
	}
	select {
	case <-a.telegram.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
