package serverapp

import (
	"context"

	"smalux-speedtest/internal/store"
	"smalux-speedtest/internal/telegrambot"
)

// telegramAuthorization 把 SQLite 授权表适配为 Bot 的权限管理端口。
// 身份判断始终使用 Telegram 数字 ID；可变的用户名和显示名只作为管理列表元数据。
type telegramAuthorization struct {
	store *store.Store
}

func (a telegramAuthorization) IsAuthorized(ctx context.Context, telegramID int64) (bool, error) {
	return a.store.IsTelegramAuthorized(ctx, telegramID)
}

func (a telegramAuthorization) IsOwner(ctx context.Context, telegramID int64) (bool, error) {
	return a.store.IsTelegramOwner(ctx, telegramID)
}

func (a telegramAuthorization) AuthorizeUser(ctx context.Context, user telegrambot.AuthorizedUser) error {
	_, err := a.store.AuthorizeTelegramUser(ctx, user.TelegramID, user.Username, user.DisplayName)
	return err
}

func (a telegramAuthorization) RevokeUser(ctx context.Context, telegramID int64) error {
	return a.store.RevokeTelegramUser(ctx, telegramID)
}

func (a telegramAuthorization) ListUsers(ctx context.Context) ([]telegrambot.AuthorizedUser, error) {
	users, err := a.store.ListTelegramUsers(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]telegrambot.AuthorizedUser, 0, len(users))
	for _, user := range users {
		result = append(result, telegrambot.AuthorizedUser{
			TelegramID:  user.TelegramID,
			Username:    user.Username,
			DisplayName: user.DisplayName,
			Owner:       user.Owner,
		})
	}
	return result, nil
}

var _ telegrambot.AuthorizationManager = telegramAuthorization{}
