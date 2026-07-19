package wire

import (
	"encoding/json"

	"smalux-speedtest/internal/model"
)

const (
	// MaxClientToServerMessageBytes 限制 Client 上报的单条消息。结果和进度都远小于
	// 2 MiB；保留该上限可约束持有合法 Token 的异常 Client 占用服务端内存。
	MaxClientToServerMessageBytes = 2 << 20
	// MaxServerToClientMessageBytes 与已发布的协议版本 1 Client 读上限保持一致。
	// 任务创建前会以相同常量检查最终 Envelope，避免新 Server 向旧 Client 下发其
	// 无法读取的消息。若以后需要扩大上限，应升级协议版本或在 hello 中显式协商。
	MaxServerToClientMessageBytes = 2 << 20

	// TypeHello 由客户端在连接建立后发送，载荷为 model.Hello。
	TypeHello = "client.hello"
	// TypeWelcome 由服务端确认客户端身份和协议版本，载荷为 model.Welcome。
	TypeWelcome = "server.welcome"
	// TypePing 是客户端的应用层保活请求。
	TypePing = "ping"
	// TypePong 是服务端对 TypePing 的响应。
	TypePong = "pong"
	// TypeTaskAssign 下发测速任务，载荷为 model.Assignment。
	TypeTaskAssign = "task.assign"
	// TypeTaskAck 表示客户端已开始处理任务，载荷为 model.Ack。
	TypeTaskAck = "task.ack"
	// TypeTaskProgress 回传非持久化的实时阶段或速率，载荷为 model.Progress。
	TypeTaskProgress = "task.progress"
	// TypeTaskResult 回传一条可持久化测速结果，载荷为 model.SpeedResult。
	TypeTaskResult = "task.result"
	// TypeTaskComplete 表示任务完整成功结束，载荷为 model.Ack。
	TypeTaskComplete = "task.complete"
	// TypeTaskFailed 表示任务因超时、取消或执行错误而未完整结束，载荷为 model.Failure。
	TypeTaskFailed = "task.failed"
	// TypeTaskCancel 要求客户端取消指定任务；TaskID 位于 Envelope 中。
	TypeTaskCancel = "task.cancel"
)

// Envelope 是所有 WebSocket 业务消息共享的外层结构。
//
// 固定元数据与具体载荷分离后，读取方可以在不知道 Payload 类型时先完成版本过滤、
// 路由和任务关联，再按 Type 选择对应 model 类型解码。
type Envelope struct {
	// Version 是 model.ProtocolVersion，用于拒绝不兼容的消息结构。
	Version int `json:"version"`
	// Type 决定 Payload 的具体类型和消息处理分支。
	Type string `json:"type"`
	// MessageID 是每次构造消息时生成的唯一标识，可用于日志追踪和未来的去重机制。
	MessageID string `json:"message_id"`
	// TaskID 关联任务消息；握手和心跳消息通常为空。
	TaskID string `json:"task_id,omitempty"`
	// Payload 保存尚未按业务类型解码的 JSON。omitempty 允许无载荷控制消息省略该字段。
	Payload json.RawMessage `json:"payload,omitempty"`
}

// New 把任意可 JSON 编码的 payload 封装为当前协议版本的 Envelope，并生成 MessageID。
// payload 编码失败时返回零值 Envelope 和原始 json.Marshal 错误。
func New(messageType, taskID string, payload any) (Envelope, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{
		Version:   model.ProtocolVersion,
		Type:      messageType,
		MessageID: model.NewID(),
		TaskID:    taskID,
		Payload:   raw,
	}, nil
}

// EncodedSize 返回 Envelope 作为 WebSocket JSON 文本消息时的字节数。
// 任务服务用它在持久化前执行与 Client SetReadLimit 一致的精确尺寸检查。
func EncodedSize(message Envelope) (int, error) {
	encoded, err := json.Marshal(message)
	return len(encoded), err
}

// Decode 将 Envelope.Payload 解码为调用方指定的类型 T。
//
// Decode 只负责 JSON 反序列化，不校验 Version、Type、TaskID 一致性或业务字段范围；
// 这些检查依赖消息上下文，应由调用方在解码前后完成。
func Decode[T any](message Envelope) (T, error) {
	var value T
	err := json.Unmarshal(message.Payload, &value)
	return value, err
}
