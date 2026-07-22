package serverapp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"smalux-speedtest/internal/logsafe"
	"smalux-speedtest/internal/store"
	"smalux-speedtest/internal/telegrambot"
)

// telegramRuntime 保存 Bot 后台循环的取消与完成信号，确保关闭 SQLite 前所有 Bot
// 任务都已停止访问任务和授权数据。
type telegramRuntime struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// initializeTelegram 优先兼容启动参数；未提供参数时从网页保存的加密配置恢复 Bot。
func (a *App) initializeTelegram(parent context.Context) error {
	if parent == nil {
		parent = context.Background()
	}
	a.telegramParent = parent
	token := strings.TrimSpace(a.config.TelegramBotToken)
	ownerID := a.config.TelegramOwnerID
	if token == "" && ownerID != 0 {
		return errors.New("telegram bot token is required when telegram owner ID is configured")
	}
	if token != "" && ownerID <= 0 {
		return errors.New("telegram owner ID must be positive when telegram bot is enabled")
	}
	if token != "" {
		return a.startTelegramRuntime(parent, token, ownerID)
	}
	stored, cipher, err := a.store.LoadTelegramConfig(parent)
	if errors.Is(err, store.ErrTelegramConfigMissing) || (err == nil && !stored.Enabled) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load telegram configuration: %w", err)
	}
	key, err := loadOrCreateTelegramKey(a.config.DatabasePath)
	if err != nil {
		a.config.Logger.Warn("telegram bot configuration requires rebinding", "error_type", logsafe.ErrorType(err))
		return nil
	}
	a.telegramKey, a.telegramKeyReady = key, true
	token, err = decryptTelegramToken(key, cipher)
	if err != nil {
		a.config.Logger.Warn("telegram bot configuration requires rebinding", "error_type", logsafe.ErrorType(err))
		return nil
	}
	if err := a.startTelegramRuntime(parent, token, stored.OwnerTelegramID); err != nil {
		a.config.Logger.Warn("telegram bot did not start", "error_type", logsafe.ErrorType(err))
	}
	return nil
}

// startTelegramRuntime 同步唯一 owner，并启动 Telegram getUpdates 长轮询。
// Token 只交给 HTTPAPI，任何日志都不会包含 Token 或代理提交内容。
func (a *App) startTelegramRuntime(parent context.Context, token string, ownerID int64) error {
	botID, err := telegramBotIDFromToken(token)
	if err != nil {
		return fmt.Errorf("initialize telegram update offset: %w", err)
	}
	api, err := telegrambot.NewHTTPAPI(telegrambot.HTTPConfig{
		Token: token, BaseURL: a.config.TelegramAPIBaseURL, Client: a.config.TelegramHTTPClient,
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
	a.telegramMu.Lock()
	if a.telegram != nil {
		select {
		case <-a.telegram.done:
			a.telegram = nil
		default:
			a.telegramMu.Unlock()
			cancel()
			return errors.New("telegram bot is already running")
		}
	}
	a.telegram = runtime
	a.telegramMu.Unlock()
	go func() {
		defer close(runtime.done)
		if err := bot.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			a.config.Logger.Error("telegram bot stopped", "error_type", logsafe.ErrorType(err))
		}
	}()
	a.config.Logger.Info("telegram bot enabled", "owner_user_id", ownerID)
	return nil
}

// cancelTelegram 只发出停止信号，使 HTTP Shutdown 可以与 Bot 清理并行等待。
func (a *App) cancelTelegram() {
	a.telegramMu.Lock()
	runtime := a.telegram
	a.telegramMu.Unlock()
	if runtime != nil {
		runtime.cancel()
	}
}

// waitTelegram 等待长轮询和所有结果 goroutine 退出，并受 Shutdown 截止时间约束。
func (a *App) waitTelegram(ctx context.Context) error {
	a.telegramMu.Lock()
	runtime := a.telegram
	a.telegramMu.Unlock()
	if runtime == nil {
		return nil
	}
	select {
	case <-runtime.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// stopTelegramRuntime 用于网页关闭或重新绑定，等待旧轮询完全退出后清除运行时。
func (a *App) stopTelegramRuntime(ctx context.Context) error {
	a.telegramMu.Lock()
	runtime := a.telegram
	if runtime == nil {
		a.telegramMu.Unlock()
		return nil
	}
	runtime.cancel()
	a.telegramMu.Unlock()
	select {
	case <-runtime.done:
		a.telegramMu.Lock()
		if a.telegram == runtime {
			a.telegram = nil
		}
		a.telegramMu.Unlock()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
