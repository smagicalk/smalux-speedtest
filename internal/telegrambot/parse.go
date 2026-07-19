package telegrambot

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"
)

// splitCommand 识别 /command、/command@botname，并保留命令后的原始多行载荷。
func splitCommand(text string) (command, payload string, ok bool) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return "", "", false
	}
	end := strings.IndexAny(text, " \t\r\n")
	head := text
	if end >= 0 {
		head = text[:end]
		payload = strings.TrimSpace(text[end:])
	}
	head = strings.TrimPrefix(head, "/")
	if at := strings.IndexByte(head, '@'); at >= 0 {
		head = head[:at]
	}
	return strings.ToLower(head), payload, true
}

// parseSubmission 把命令或普通文本分类为代理源和订阅 URL。
func parseSubmission(command, payload string, isCommand bool, original string) (source, subscriptionURL string, err error) {
	if isCommand {
		switch command {
		case "test", "speedtest":
			if payload == "" {
				return "", "", errors.New("请在 /test 后附上代理链接或批量文本。")
			}
			return payload, "", nil
		case "sub", "subscription":
			if !isHTTPURL(payload) {
				return "", "", errors.New("请在 /sub 后提供有效的 HTTP(S) 订阅地址。")
			}
			return "", payload, nil
		default:
			return "", "", errors.New("未知命令。使用 /help 查看测速用法。")
		}
	}
	original = strings.TrimSpace(original)
	if original == "" {
		return "", "", errors.New("请发送代理链接、批量文本或订阅地址。")
	}
	return original, "", nil
}

// isHTTPURL 严格要求绝对 HTTP(S) URL 且主机非空。
func isHTTPURL(value string) bool {
	parsed, err := url.ParseRequestURI(strings.TrimSpace(value))
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != ""
}

// principalFromMessage 不信任消息正文中的身份信息，只使用 Telegram Update 的 From/Chat。
func principalFromMessage(message *Message) Principal {
	principal := Principal{ChatID: message.Chat.ID}
	if message.From != nil {
		principal.UserID = message.From.ID
		principal.Username = message.From.Username
		principal.FirstName = message.From.FirstName
		principal.LastName = message.From.LastName
	}
	return principal
}

// waitContext 等待重试间隔，同时允许 Context 立即终止等待。
func waitContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// truncateRunes 按 Unicode 码点限制 Telegram 文本长度，避免截断 UTF-8 字节序列。
func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}
