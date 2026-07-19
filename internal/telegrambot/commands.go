package telegrambot

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// handleAuthorizationCommand 执行 owner-only 授权名单管理；普通授权用户也无法调用。
func (b *Bot) handleAuthorizationCommand(ctx context.Context, message *Message, principal Principal, command, payload string) {
	owner, err := b.authorization.IsOwner(ctx, principal.UserID)
	if err != nil {
		b.config.Logger.Warn("telegram owner check failed", "user_id", principal.UserID, "error", err)
		b.sendText(ctx, message.Chat.ID, "所有者权限校验失败，请稍后重试。")
		return
	}
	if !owner {
		b.sendText(ctx, message.Chat.ID, "该命令仅 Bot 所有者可用。")
		return
	}
	if command == "users" {
		users, err := b.authorization.ListUsers(ctx)
		if err != nil {
			b.config.Logger.Warn("telegram authorized user list failed", "error", err)
			b.sendText(ctx, message.Chat.ID, "读取授权名单失败。")
			return
		}
		sort.Slice(users, func(i, j int) bool { return users[i].TelegramID < users[j].TelegramID })
		var output strings.Builder
		output.WriteString("已授权用户：")
		if len(users) == 0 {
			output.WriteString("\n（空）")
		}
		for _, user := range users {
			fmt.Fprintf(&output, "\n- %d", user.TelegramID)
			if user.Username != "" {
				fmt.Fprintf(&output, " @%s", user.Username)
			}
			if user.DisplayName != "" {
				fmt.Fprintf(&output, " (%s)", user.DisplayName)
			}
			if user.Owner {
				output.WriteString(" [owner]")
			}
		}
		b.sendText(ctx, message.Chat.ID, output.String())
		return
	}
	target, err := authorizationTarget(message, payload)
	if err != nil {
		b.sendText(ctx, message.Chat.ID, err.Error())
		return
	}
	if command == "authorize" {
		if err := b.authorization.AuthorizeUser(ctx, target); err != nil {
			b.config.Logger.Warn("telegram user authorization failed", "target_user_id", target.TelegramID, "error", err)
			b.sendText(ctx, message.Chat.ID, "授权用户失败。")
			return
		}
		b.config.Logger.Info("telegram user authorized", "owner_user_id", principal.UserID, "target_user_id", target.TelegramID)
		b.sendText(ctx, message.Chat.ID, fmt.Sprintf("已授权用户 %d。", target.TelegramID))
		return
	}
	targetIsOwner, err := b.authorization.IsOwner(ctx, target.TelegramID)
	if err != nil {
		b.sendText(ctx, message.Chat.ID, "检查目标用户权限失败。")
		return
	}
	if targetIsOwner {
		b.sendText(ctx, message.Chat.ID, "不能通过 Bot 撤销 owner。")
		return
	}
	if err := b.authorization.RevokeUser(ctx, target.TelegramID); err != nil {
		b.config.Logger.Warn("telegram user revoke failed", "target_user_id", target.TelegramID, "error", err)
		b.sendText(ctx, message.Chat.ID, "撤销用户失败。")
		return
	}
	b.config.Logger.Info("telegram user revoked", "owner_user_id", principal.UserID, "target_user_id", target.TelegramID)
	b.sendText(ctx, message.Chat.ID, fmt.Sprintf("已撤销用户 %d。", target.TelegramID))
}

// handleCancel 仅允许已授权用户取消自己的当前任务。
func (b *Bot) handleCancel(ctx context.Context, chatID int64, principal Principal) {
	allowed, err := b.authorization.IsAuthorized(ctx, principal.UserID)
	if err != nil || !allowed {
		b.sendText(ctx, chatID, "当前账号未获准管理测速任务。")
		return
	}
	taskID, claim := b.claimCancellation(principal.UserID)
	switch claim {
	case cancellationNoTask:
		b.sendText(ctx, chatID, "当前没有进行中的测速任务。")
		return
	case cancellationCreating:
		b.sendText(ctx, chatID, "任务正在创建，请稍后再试。")
		return
	case cancellationAlreadyStarted:
		b.sendText(ctx, chatID, fmt.Sprintf("任务 %s 正在取消，请勿重复提交。", taskID))
		return
	}
	canceler, ok := b.runner.(Canceler)
	if !ok {
		b.clearCancellation(principal.UserID, taskID)
		b.sendText(ctx, chatID, "当前任务执行器不支持取消。")
		return
	}
	// Hub 取消可能需要通知多个远程 Client，放到受 Run 管理的 goroutine
	// 中执行，避免阻塞 getUpdates 主循环与其他授权命令。
	b.wg.Add(1)
	go b.cancelAndReply(ctx, chatID, principal.UserID, taskID, canceler)
}

func (b *Bot) cancelAndReply(ctx context.Context, chatID, userID int64, taskID string, canceler Canceler) {
	defer b.wg.Done()
	if err := canceler.Cancel(ctx, taskID); err != nil {
		b.clearCancellation(userID, taskID)
		b.config.Logger.Warn("telegram task cancellation failed", "task_id", taskID, "user_id", userID, "error_type", fmt.Sprintf("%T", err))
		b.sendText(ctx, chatID, fmt.Sprintf("任务 %s 取消失败。", taskID))
		return
	}
	b.sendText(ctx, chatID, fmt.Sprintf("已请求取消任务 %s。", taskID))
}

// authorizationTarget 优先解析命令参数中的数字 ID；参数为空时使用被回复消息的发送者。
func authorizationTarget(message *Message, payload string) (AuthorizedUser, error) {
	payload = strings.TrimSpace(payload)
	if payload != "" {
		fields := strings.Fields(payload)
		if len(fields) != 1 {
			return AuthorizedUser{}, errors.New("请提供一个 Telegram user_id，或回复目标用户的消息。")
		}
		id, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil || id <= 0 {
			return AuthorizedUser{}, errors.New("Telegram user_id 必须是正整数。")
		}
		return AuthorizedUser{TelegramID: id}, nil
	}
	if message.ReplyToMessage == nil || message.ReplyToMessage.From == nil || message.ReplyToMessage.From.ID <= 0 {
		return AuthorizedUser{}, errors.New("请提供 Telegram user_id，或回复目标用户的消息。")
	}
	user := message.ReplyToMessage.From
	return AuthorizedUser{
		TelegramID:  user.ID,
		Username:    user.Username,
		DisplayName: strings.TrimSpace(strings.TrimSpace(user.FirstName) + " " + strings.TrimSpace(user.LastName)),
	}, nil
}
