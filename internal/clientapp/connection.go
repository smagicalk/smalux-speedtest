package clientapp

import (
	"context"
	"sync"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"smalux-speedtest/internal/wire"
)

// connection 封装单次 WebSocket 连接及其任务并发状态。
//
// coder/websocket 允许读写并发，但不允许多个 writer 同时写，因此 writeMu 覆盖所有
// wsjson.Write。current 同时保护当前任务的取消函数和 canceled 集合；后者用于记住
// “取消消息先于 worker 开始任务”这一竞态，防止已进入 assignments 队列的任务仍被执行。
type connection struct {
	// ws 是本次连接的底层 WebSocket，会在 connect 返回时关闭。
	ws *websocket.Conn
	// writeMu 串行化心跳、worker 和取消处理产生的所有消息写入。
	writeMu sync.Mutex

	// current 保护 cancel、taskID 和 canceled 三个任务状态字段。
	current sync.Mutex
	// cancel 终止当前正在执行的 Assignment；空值表示 worker 空闲。
	cancel context.CancelFunc
	// taskID 标识 cancel 当前对应的任务。
	taskID string
	// canceled 记录先于 worker 启动到达的取消消息，消费后删除。
	canceled map[string]bool
}

// send 是 connection 唯一的 WebSocket 写入口。writeMu 保证心跳、任务进度和任务结果
// 不会并发调用 wsjson.Write；ctx 则为每类消息提供各自的写入截止时间。
func (c *connection) send(ctx context.Context, message wire.Envelope) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return wsjson.Write(ctx, c.ws, message)
}

// setCurrent 发布当前任务及其取消函数，使读取循环收到 task.cancel 时能够中断 worker。
func (c *connection) setCurrent(taskID string, cancel context.CancelFunc) {
	c.current.Lock()
	c.taskID, c.cancel = taskID, cancel
	c.current.Unlock()
}

// clearCurrent 仅在 taskID 仍匹配时清理状态，避免迟到的旧任务收尾覆盖新任务状态。
func (c *connection) clearCurrent(taskID string) {
	c.current.Lock()
	if c.taskID == taskID {
		c.taskID, c.cancel = "", nil
	}
	c.current.Unlock()
}

// cancelTask 记录取消意图并取消同 ID 的运行中任务。即使任务还在队列中，canceled
// 标记也会由 worker 在执行前识别。
func (c *connection) cancelTask(taskID string) {
	c.current.Lock()
	c.canceled[taskID] = true
	if c.taskID == taskID && c.cancel != nil {
		c.cancel()
	}
	c.current.Unlock()
}

// consumeCanceled 原子地查询并消费排队任务的取消标记。
func (c *connection) consumeCanceled(taskID string) bool {
	c.current.Lock()
	defer c.current.Unlock()
	if !c.canceled[taskID] {
		return false
	}
	delete(c.canceled, taskID)
	return true
}

// cancelCurrent 在连接读失败时中断当前测速，防止失去控制通道后仍持续消耗带宽。
func (c *connection) cancelCurrent() {
	c.current.Lock()
	if c.cancel != nil {
		c.cancel()
	}
	c.current.Unlock()
}
