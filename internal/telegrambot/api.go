package telegrambot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const defaultTelegramBaseURL = "https://api.telegram.org"

// HTTPConfig 配置 Telegram Bot HTTP API 客户端。
type HTTPConfig struct {
	// Token 是 BotFather 签发的 Bot Token，不能为空。
	Token string
	// BaseURL 默认使用 Telegram 官方地址；自建远程服务必须使用 HTTPS，loopback 可用 HTTP。
	BaseURL string
	// Client 可注入定制 Transport。为 nil 时使用带 65 秒总超时且禁止重定向的客户端。
	// 注入 Client 时调用方应自行保证其 CheckRedirect 不会把 Token 转发到不可信主机。
	Client *http.Client
}

// HTTPAPI 使用标准库实现 Telegram Bot API 的轮询、菜单、按钮、消息编辑和图片发送。
type HTTPAPI struct {
	token   string
	baseURL string
	client  *http.Client
}

// APIError 表示 Telegram 返回的非成功业务响应或 HTTP 状态。
type APIError struct {
	Method     string
	StatusCode int
	ErrorCode  int
	// RetryAfterSeconds 是 Telegram 限流响应建议的等待秒数；没有该参数时为 0。
	RetryAfterSeconds int
	Description       string
}

// Error 返回不包含 Bot Token 的诊断文本。
func (e *APIError) Error() string {
	if e.Description != "" {
		return fmt.Sprintf("telegram %s failed (http=%d, code=%d): %s", e.Method, e.StatusCode, e.ErrorCode, e.Description)
	}
	return fmt.Sprintf("telegram %s failed (http=%d, code=%d)", e.Method, e.StatusCode, e.ErrorCode)
}

// NewHTTPAPI 校验配置并创建 Telegram HTTP 客户端。
func NewHTTPAPI(config HTTPConfig) (*HTTPAPI, error) {
	token := strings.TrimSpace(config.Token)
	if token == "" {
		return nil, errors.New("telegram bot token is required")
	}
	baseURL := strings.TrimRight(strings.TrimSpace(config.BaseURL), "/")
	if baseURL == "" {
		baseURL = defaultTelegramBaseURL
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, errors.New("telegram base URL must be an absolute HTTP(S) URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("telegram base URL must not contain credentials, query parameters or a fragment")
	}
	if parsed.Scheme == "http" && !isLoopbackTelegramHost(parsed.Hostname()) {
		return nil, errors.New("telegram base URL must use HTTPS unless it points to loopback")
	}
	client := config.Client
	if client == nil {
		// Telegram long polling 通常使用 25 秒 timeout；65 秒给连接建立和响应传输留出余量。
		// Bot Token 位于 URL 路径，Telegram API 不需要重定向，直接拒绝可避免跨主机泄露。
		client = &http.Client{Timeout: 65 * time.Second, CheckRedirect: rejectTelegramRedirect}
	} else if client.CheckRedirect == nil {
		// 不修改调用方拥有的 Client，但为常见的“只注入 Transport”用法补上安全默认值。
		clone := *client
		clone.CheckRedirect = rejectTelegramRedirect
		client = &clone
	}
	return &HTTPAPI{token: token, baseURL: baseURL, client: client}, nil
}

func rejectTelegramRedirect(_ *http.Request, _ []*http.Request) error {
	return http.ErrUseLastResponse
}

// isLoopbackTelegramHost 只为同机测试或本地 Bot API Server 放行明文 HTTP。
// 其他自建端点必须使用 HTTPS，因为 Bot Token 位于每个 API 请求的 URL 路径中。
func isLoopbackTelegramHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}

// GetUpdates 调用 Telegram getUpdates，并只请求 message 类型更新。
func (a *HTTPAPI) GetUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]Update, error) {
	seconds := int64((timeout + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	request := struct {
		Offset         int64    `json:"offset"`
		Timeout        int64    `json:"timeout"`
		AllowedUpdates []string `json:"allowed_updates"`
	}{Offset: offset, Timeout: seconds, AllowedUpdates: []string{"message", "callback_query"}}
	var updates []Update
	if err := a.callJSON(ctx, "getUpdates", request, &updates); err != nil {
		return nil, err
	}
	return updates, nil
}

func (a *HTTPAPI) SetMyCommands(ctx context.Context, commands []BotCommand) error {
	return a.callJSON(ctx, "setMyCommands", struct {
		Commands []BotCommand `json:"commands"`
	}{Commands: commands}, nil)
}

// SendMessage 通过 sendMessage 发送不启用 parse_mode 的纯文本。
func (a *HTTPAPI) SendMessage(ctx context.Context, request MessageRequest) (SentMessage, error) {
	var result SentMessage
	err := a.callJSON(ctx, "sendMessage", request, &result)
	return result, err
}

func (a *HTTPAPI) EditMessageText(ctx context.Context, request EditMessageRequest) error {
	return a.callJSON(ctx, "editMessageText", request, nil)
}

func (a *HTTPAPI) AnswerCallbackQuery(ctx context.Context, callbackQueryID, text string) error {
	request := struct {
		CallbackQueryID string `json:"callback_query_id"`
		Text            string `json:"text,omitempty"`
	}{CallbackQueryID: callbackQueryID, Text: truncateRunes(text, 200)}
	return a.callJSON(ctx, "answerCallbackQuery", request, nil)
}

func (a *HTTPAPI) DeleteMessage(ctx context.Context, chatID, messageID int64) error {
	request := struct {
		ChatID    int64 `json:"chat_id"`
		MessageID int64 `json:"message_id"`
	}{ChatID: chatID, MessageID: messageID}
	return a.callJSON(ctx, "deleteMessage", request, nil)
}

// SendPhoto 使用 multipart/form-data 上传内存图片。
func (a *HTTPAPI) SendPhoto(ctx context.Context, request PhotoRequest) error {
	if len(request.Data) == 0 {
		return errors.New("telegram photo data is empty")
	}
	filename := safeFilename(request.Filename)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("chat_id", fmt.Sprintf("%d", request.ChatID)); err != nil {
		return err
	}
	if request.Caption != "" {
		if err := writer.WriteField("caption", request.Caption); err != nil {
			return err
		}
	}
	if request.ReplyParameters != nil {
		value, err := json.Marshal(request.ReplyParameters)
		if err != nil {
			return err
		}
		if err := writer.WriteField("reply_parameters", string(value)); err != nil {
			return err
		}
	}
	part, err := writer.CreateFormFile("photo", filename)
	if err != nil {
		return err
	}
	if _, err := part.Write(request.Data); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, a.methodURL("sendPhoto"), &body)
	if err != nil {
		return fmt.Errorf("create telegram sendPhoto request: %s", a.redact(err.Error()))
	}
	httpRequest.Header.Set("Content-Type", writer.FormDataContentType())
	return a.execute(httpRequest, "sendPhoto", nil)
}

// callJSON 编码一个 JSON 请求并解码 Telegram 的统一响应信封。
func (a *HTTPAPI) callJSON(ctx context.Context, method string, input, output any) error {
	payload, err := json.Marshal(input)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.methodURL(method), bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("create telegram %s request: %s", method, a.redact(err.Error()))
	}
	request.Header.Set("Content-Type", "application/json")
	return a.execute(request, method, output)
}
