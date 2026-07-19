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
	// SendMessage 向指定会话发送纯文本消息。本包不启用 HTML/Markdown 解析模式。
	SendMessage(ctx context.Context, chatID int64, text string) error
	// SendPhoto 向指定会话上传内存图片；filename 仅作为 Telegram 文件名元数据。
	SendPhoto(ctx context.Context, chatID int64, filename, caption string, data []byte) error
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
	UpdateID int64    `json:"update_id"`
	Message  *Message `json:"message,omitempty"`
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
// Source 和 SubscriptionURL 互斥；Runner 适配器负责补充目标 Client、候选数、TopN 和线程数。
type TaskRequest struct {
	Principal       Principal
	Source          string
	SubscriptionURL string
}

// Task 是 Runner 创建成功后的最小任务引用。
type Task struct {
	ID string
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
	Filename string
	Caption  string
	Data     []byte
}
