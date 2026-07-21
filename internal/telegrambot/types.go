package telegrambot

import (
	"context"
	"errors"
	"strings"
	"time"
)

// API 是 Bot 使用的最小 Telegram Bot API 集合。
// HTTPAPI 是生产实现；测试可以注入内存实现而无需访问 Telegram 网络。
type API interface {
	// GetUpdates 从 offset 开始长轮询消息更新。成功返回的 UpdateID 应由调用方推进 offset。
	GetUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]Update, error)
	// SetMyCommands 注册 Telegram 输入框菜单中的命令提示。
	SetMyCommands(ctx context.Context, commands []BotCommand) error
	// SendMessage 发送文本并返回 Telegram 分配的消息 ID，供后续进度编辑使用。
	SendMessage(ctx context.Context, request MessageRequest) (SentMessage, error)
	// EditMessageText 原地更新 Bot 已发送的配置或进度消息。
	EditMessageText(ctx context.Context, request EditMessageRequest) error
	// AnswerCallbackQuery 及时结束客户端按钮的加载状态。
	AnswerCallbackQuery(ctx context.Context, callbackQueryID, text string) error
	// DeleteMessage 删除已被最终结果图片替代的临时控制/进度消息。
	DeleteMessage(ctx context.Context, chatID, messageID int64) error
	// SendPhoto 向指定会话上传内存图片，并可引用原始测速请求。
	SendPhoto(ctx context.Context, request PhotoRequest) error
}

// BotCommand 是 Telegram 菜单中的一条 slash command。
type BotCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

// InlineKeyboardMarkup 和 InlineKeyboardButton 描述消息下方的回调按钮。
type InlineKeyboardMarkup struct {
	InlineKeyboard [][]InlineKeyboardButton `json:"inline_keyboard"`
}

type InlineKeyboardButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

// ReplyParameters 让 Bot 的状态和图片明确引用触发测速的用户消息。
type ReplyParameters struct {
	MessageID                int64 `json:"message_id"`
	AllowSendingWithoutReply bool  `json:"allow_sending_without_reply,omitempty"`
}

type MessageRequest struct {
	ChatID          int64                 `json:"chat_id"`
	Text            string                `json:"text"`
	ReplyParameters *ReplyParameters      `json:"reply_parameters,omitempty"`
	ReplyMarkup     *InlineKeyboardMarkup `json:"reply_markup,omitempty"`
}

type EditMessageRequest struct {
	ChatID      int64                 `json:"chat_id"`
	MessageID   int64                 `json:"message_id"`
	Text        string                `json:"text"`
	ReplyMarkup *InlineKeyboardMarkup `json:"reply_markup,omitempty"`
}

type SentMessage struct {
	MessageID int64 `json:"message_id"`
}

type PhotoRequest struct {
	ChatID          int64
	Filename        string
	Caption         string
	Data            []byte
	ReplyParameters *ReplyParameters
}

// AuthorizationManager 提供任务授权检查和 owner 专用的授权名单管理。
//
// 接口刻意使用 Telegram ID 和普通值对象，不依赖 store 的具体类型；服务端适配器可以
// 直接转调 IsTelegramAuthorized、IsTelegramOwner、AuthorizeTelegramUser、
// RevokeTelegramUser 和 ListTelegramUsers 等持久化方法。
type AuthorizationManager interface {
	IsAuthorized(ctx context.Context, telegramID int64) (bool, error)
	IsOwner(ctx context.Context, telegramID int64) (bool, error)
	AuthorizeUser(ctx context.Context, user AuthorizedUser) error
	RevokeUser(ctx context.Context, telegramID int64) error
	ListUsers(ctx context.Context) ([]AuthorizedUser, error)
}

// UpdateOffsetStore 持久化 Telegram getUpdates 的下一个 offset。
// Bot 在处理更新前先保存 offset，实现跨进程的 at-most-once 消息处理：
// 崩溃时用户最多需要重新发送，但不会自动重复创建高流量测速任务。
type UpdateOffsetStore interface {
	LoadTelegramUpdateOffset(ctx context.Context) (int64, error)
	SaveTelegramUpdateOffset(ctx context.Context, offset int64) error
}

// AuthorizedUser 是授权名单中一条与存储实现无关的记录。
type AuthorizedUser struct {
	TelegramID  int64
	Username    string
	DisplayName string
	Owner       bool
}

// Runner 把 Telegram 请求桥接到实际测速任务系统。
//
// Submit 应尽快完成任务创建并返回稳定 ID，不应等待测速结束。Wait 应阻塞到任务进入
// 终态或 ctx 取消；Bot 会在独立 goroutine 调用它，因此后续 long polling 不受影响。
type Runner interface {
	Submit(ctx context.Context, request TaskRequest) (Task, error)
	Wait(ctx context.Context, taskID string) (Completion, error)
}

// UserError 标记可以安全回传给 Telegram 用户的任务创建错误。
// Runner 适配器可用它暴露“无在线 Client”“订阅抓取失败”“参数越界”或不含原文的解析
// 摘要；普通 error 被视为内部错误，Bot 不会把其文本发送到 Telegram。
type UserError interface {
	error
	UserMessage() string
}

// VisibleError 是 UserError 的简单值实现。
type VisibleError struct {
	Message string
}

// Error 实现 error。
func (e VisibleError) Error() string { return e.Message }

// UserMessage 返回经过集成适配器确认可公开展示的文本。
func (e VisibleError) UserMessage() string { return e.Message }

// NewUserError 创建一个可安全展示的错误；空消息会回退为通用任务创建错误。
func NewUserError(message string) error {
	message = strings.TrimSpace(message)
	if message == "" {
		message = "任务创建失败。"
	}
	return VisibleError{Message: message}
}

// UserMessage 从错误链中提取安全消息；未实现 UserError 时返回空串。
func UserMessage(err error) string {
	var visible UserError
	if err != nil && errors.As(err, &visible) {
		return strings.TrimSpace(visible.UserMessage())
	}
	return ""
}

// Canceler 是 Runner 的可选扩展。Bot 在处理 /cancel 时进行接口断言，因此不支持取消的
// Runner 仍可用于基础提交与等待流程。
type Canceler interface {
	Cancel(ctx context.Context, taskID string) error
}

// Renderer 把已完成或部分完成任务渲染为 Telegram 可上传的图片。
// 实现可以根据 Completion.TaskID 查询 store，也可以使用 Completion 中的扩展载荷。
type Renderer interface {
	Render(ctx context.Context, completion Completion) (Image, error)
}

// Update 是 getUpdates 响应中本包关心的最小更新结构。
// 非 message 更新会被 Bot 忽略，但其 UpdateID 仍会推进，避免重复拉取。
type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *Message       `json:"message,omitempty"`
	CallbackQuery *CallbackQuery `json:"callback_query,omitempty"`
}

type CallbackQuery struct {
	ID      string   `json:"id"`
	From    User     `json:"from"`
	Message *Message `json:"message,omitempty"`
	Data    string   `json:"data,omitempty"`
}

// Message 是 Telegram 文本消息的最小投影。
type Message struct {
	MessageID      int64    `json:"message_id"`
	From           *User    `json:"from,omitempty"`
	Chat           Chat     `json:"chat"`
	Text           string   `json:"text,omitempty"`
	ReplyToMessage *Message `json:"reply_to_message,omitempty"`
}

// User 描述消息发送者。频道消息可能没有 From，届时 Principal.UserID 为 0。
type User struct {
	ID        int64  `json:"id"`
	Username  string `json:"username,omitempty"`
	FirstName string `json:"first_name,omitempty"`
	LastName  string `json:"last_name,omitempty"`
}

// Chat 标识 Telegram 私聊、群组或频道。
type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type,omitempty"`
}

// Principal 是交给 Authorizer 的稳定身份信息。
type Principal struct {
	UserID    int64
	ChatID    int64
	Username  string
	FirstName string
	LastName  string
}

// TaskRequest 是 Telegram 消息解析后的任务创建请求。
// Source 和 SubscriptionURL 可以同时提供；Runner 适配器会合并两路来源，并负责补充
// 目标 Client、候选数、TopN 和线程数。
type TaskRequest struct {
	Principal       Principal
	Source          string
	SubscriptionURL string
	CandidateCount  int
	TopN            int
	Threads         int
	ClientIDs       []string
}

// ClientOption 是 Bot 节点选择器可展示的在线 Client。
type ClientOption struct {
	ID   string
	Name string
}

// ClientProvider 是 Runner 的可选节点发现扩展。
type ClientProvider interface {
	ListAvailableClients(ctx context.Context) ([]ClientOption, error)
}

// TaskProgress 是 Bot 可公开展示的脱敏任务进度。
type TaskProgress struct {
	Status       string
	Phase        string
	ProxyName    string
	Message      string
	Current      int
	Total        int
	RateBPS      float64
	Results      int
	ClientID     string
	ClientName   string
	TargetStatus string
}

// ProgressRunner 是 Runner 的可选扩展。生产适配器实现它以实时编辑 Telegram
// 状态消息；简单测试 Runner 可继续只实现 Wait。
type ProgressRunner interface {
	WaitWithProgress(ctx context.Context, taskID string, onProgress func(TaskProgress)) (Completion, error)
}

// Task 是 Runner 创建成功后的最小任务引用。
type Task struct {
	ID          string
	ClientCount int
	ProxyCount  int
	TopN        int
	Clients     []ClientOption
}

// Completion 描述 Runner.Wait 返回的任务终态。
// Payload 为集成适配器保留，可携带渲染所需的任务/结果快照；Bot 本身不读取该字段。
type Completion struct {
	TaskID  string
	Status  string
	Message string
	Payload any
}

// Image 是 Renderer 生成的上传对象。
type Image struct {
	Filename        string
	Caption         string
	Data            []byte
	ReplyParameters *ReplyParameters
}
