package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

const telegramUpdateOffsetKeyPrefix = "telegram_update_offset:"

// LoadTelegramUpdateOffset 读取指定 Bot 已确认的下一个 Update ID。首次启用时
// settings 中没有记录，返回 0 以便 Telegram 从当前未确认更新开始投递。
//
// update ID 属于单个 Bot 的更新流，因此 botID 必须进入存储边界。这样服务端
// 更换 Bot 并复用同一个数据库时，不会错误继承前一个 Bot 的游标并跳过消息。
func (s *Store) LoadTelegramUpdateOffset(ctx context.Context, botID int64) (int64, error) {
	key, err := telegramUpdateOffsetKey(botID)
	if err != nil {
		return 0, err
	}
	var raw string
	err = s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	offset, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || offset < 0 {
		return 0, fmt.Errorf("invalid stored telegram update offset")
	}
	return offset, nil
}

// SaveTelegramUpdateOffset 以单调方式持久化 offset，迟到的较小值不会使 Bot
// 在重启后倒退并重复处理历史更新。
func (s *Store) SaveTelegramUpdateOffset(ctx context.Context, botID, offset int64) error {
	key, err := telegramUpdateOffsetKey(botID)
	if err != nil {
		return err
	}
	if offset < 0 {
		return errors.New("telegram update offset cannot be negative")
	}
	value := strconv.FormatInt(offset, 10)
	_, err = s.db.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES(?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value
		WHERE CAST(settings.value AS INTEGER) < CAST(excluded.value AS INTEGER)`, key, value)
	return err
}

func telegramUpdateOffsetKey(botID int64) (string, error) {
	if botID <= 0 {
		return "", errors.New("telegram bot ID must be positive")
	}
	return telegramUpdateOffsetKeyPrefix + strconv.FormatInt(botID, 10), nil
}
