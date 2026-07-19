package serverapp

import (
	"context"
	"errors"
	"strconv"
	"strings"
)

// telegramOffsetStore 把 Bot 的 offset 端口适配到与授权名单共用的 SQLite。
// 这里只持久化 Bot ID 和整数游标，不保存 Telegram 消息、Token 或代理原文。
type telegramOffsetStore struct {
	app   *App
	botID int64
}

func (s telegramOffsetStore) LoadTelegramUpdateOffset(ctx context.Context) (int64, error) {
	return s.app.store.LoadTelegramUpdateOffset(ctx, s.botID)
}

func (s telegramOffsetStore) SaveTelegramUpdateOffset(ctx context.Context, offset int64) error {
	return s.app.store.SaveTelegramUpdateOffset(ctx, s.botID, offset)
}

// telegramBotIDFromToken 从 "<bot-id>:<secret>" 中提取 Telegram 分配的稳定
// 数字 Bot ID。Token 密钥轮换不会改变 Bot ID，因此可以继续消费同一更新流。
func telegramBotIDFromToken(token string) (int64, error) {
	rawID, secret, ok := strings.Cut(strings.TrimSpace(token), ":")
	if !ok || rawID == "" || secret == "" {
		return 0, errors.New("telegram bot token must start with a positive numeric bot ID")
	}
	botID, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil || botID <= 0 {
		return 0, errors.New("telegram bot token must start with a positive numeric bot ID")
	}
	return botID, nil
}
