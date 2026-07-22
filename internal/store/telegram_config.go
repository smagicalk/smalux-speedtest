package store

import (
	"context"
	"database/sql"
	"errors"
)

// TelegramConfig 是 Bot 控制面的非敏感元数据；Token 明文永远不离开 serverapp 的
// 解密边界，也不会作为 JSON 结构返回给网页。
type TelegramConfig struct {
	BotID           int64  `json:"bot_id"`
	BotUsername     string `json:"bot_username,omitempty"`
	OwnerTelegramID int64  `json:"owner_telegram_id"`
	Enabled         bool   `json:"enabled"`
	UpdatedAt       string `json:"updated_at"`
}

var ErrTelegramConfigMissing = errors.New("telegram bot is not configured")

// SaveTelegramConfig 原子替换单例配置。cipher 必须是 app 层使用本机密钥生成的密文。
func (s *Store) SaveTelegramConfig(ctx context.Context, cipher []byte, botID int64, username string, ownerID int64, enabled bool) error {
	if len(cipher) == 0 || botID <= 0 || ownerID <= 0 {
		return errors.New("invalid telegram configuration")
	}
	flag := 0
	if enabled {
		flag = 1
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO telegram_config(id,token_cipher,bot_id,bot_username,owner_telegram_id,enabled,updated_at)
		VALUES(1,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET token_cipher=excluded.token_cipher,bot_id=excluded.bot_id,
		bot_username=excluded.bot_username,owner_telegram_id=excluded.owner_telegram_id,enabled=excluded.enabled,updated_at=excluded.updated_at`,
		cipher, botID, username, ownerID, flag, now())
	return err
}

// LoadTelegramConfig 返回网页可见元数据和加密 Token。cipher 只能交给本机密钥解密。
func (s *Store) LoadTelegramConfig(ctx context.Context) (TelegramConfig, []byte, error) {
	var config TelegramConfig
	var enabled int
	var cipher []byte
	err := s.db.QueryRowContext(ctx, `SELECT token_cipher,bot_id,bot_username,owner_telegram_id,enabled,updated_at FROM telegram_config WHERE id=1`).
		Scan(&cipher, &config.BotID, &config.BotUsername, &config.OwnerTelegramID, &enabled, &config.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return TelegramConfig{}, nil, ErrTelegramConfigMissing
	}
	if err != nil {
		return TelegramConfig{}, nil, err
	}
	config.Enabled = enabled != 0
	return config, cipher, nil
}

// SetTelegramEnabled 切换 Bot 后台运行状态，不修改 Token 或绑定身份。
func (s *Store) SetTelegramEnabled(ctx context.Context, enabled bool) error {
	flag := 0
	if enabled {
		flag = 1
	}
	result, err := s.db.ExecContext(ctx, `UPDATE telegram_config SET enabled=?,updated_at=? WHERE id=1`, flag, now())
	if err != nil {
		return err
	}
	changed, _ := result.RowsAffected()
	if changed == 0 {
		return ErrTelegramConfigMissing
	}
	return nil
}

// DeleteTelegramConfig 删除绑定并同时移除 Bot 配置；授权名单保留，便于重新绑定后继续使用。
func (s *Store) DeleteTelegramConfig(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM telegram_config WHERE id=1`)
	return err
}
