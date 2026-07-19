package serverapp

import (
	"encoding/json"
	"strings"

	"smalux-speedtest/internal/model"
)

// taskEvent 是 Hub 推送给管理页面 SSE 的内部事件表示。
// 每个事件按 Type 只使用对应载荷字段，omitempty 可减少流式消息大小。
type taskEvent struct {
	// Type 决定前端应读取 progress、result 还是 status 字段。
	Type string `json:"type"`
	// Progress 是 task.progress 事件的瞬时进度载荷。
	Progress *model.Progress `json:"progress,omitempty"`
	// Result 是成功落库后广播的单条测速结果。
	Result *model.SpeedResult `json:"result,omitempty"`
	// Status 是任务或目标聚合后的状态名称。
	Status string `json:"status,omitempty"`
	// Message 提供状态事件的可选补充说明。
	Message string `json:"message,omitempty"`
}

// Subscribe 为指定任务注册一个容量为 32 的 SSE 事件队列，并返回幂等风格的注销函数。
// channel 不会在注销时 close，因为 publish 可能已经取得其快照；不关闭可避免并发发送
// 导致 panic。HTTP handler 通过请求 Context 退出，不依赖 channel 关闭信号。
func (h *Hub) Subscribe(taskID string) (<-chan taskEvent, func()) {
	channel := make(chan taskEvent, 32)
	h.mu.Lock()
	if h.subscribers[taskID] == nil {
		h.subscribers[taskID] = make(map[chan taskEvent]struct{})
	}
	h.subscribers[taskID][channel] = struct{}{}
	h.mu.Unlock()
	return channel, func() {
		h.mu.Lock()
		delete(h.subscribers[taskID], channel)
		if len(h.subscribers[taskID]) == 0 {
			delete(h.subscribers, taskID)
		}
		h.mu.Unlock()
	}
}

// publish 将事件尽力广播给任务的当前订阅者。
//
// 先在读锁内复制 channel 列表，再在锁外执行非阻塞发送。慢浏览器队列满时会丢弃增量
// 事件而不会反压 WebSocket/SQLite 主流程；前端可通过任务 REST API 重新获取权威快照。
func (h *Hub) publish(taskID string, event taskEvent) {
	h.mu.RLock()
	channels := make([]chan taskEvent, 0, len(h.subscribers[taskID]))
	for channel := range h.subscribers[taskID] {
		channels = append(channels, channel)
	}
	h.mu.RUnlock()
	for _, channel := range channels {
		select {
		case channel <- event:
		default:
		}
	}
}

// bearerToken 从 Authorization Header 解析大小写不敏感的 Bearer 方案。
// 格式不正确时返回空串，随后统一由 store.AuthenticateClient 判定为未认证。
func bearerToken(header string) string {
	parts := strings.SplitN(header, " ", 2)
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		return strings.TrimSpace(parts[1])
	}
	return ""
}

// eventJSON 把内部事件编码为 SSE data 行的 JSON；taskEvent 只包含可编码字段，因此沿用
// 无错误返回的简化接口。
func eventJSON(event taskEvent) []byte {
	value, _ := json.Marshal(event)
	return value
}
