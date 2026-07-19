package serverapp

import (
	"context"
	"time"

	"smalux-speedtest/internal/model"
	"smalux-speedtest/internal/wire"
)

// handleMessage 按 wire.Type 驱动任务状态机。
//
// 心跳只刷新 Client 活跃时间；ACK 将 queued 转为 running；进度只广播不持久化；结果在
// 确认目标仍为 running 后持久化；complete/failed 则完成单个目标并触发任务聚合。无法
// 解码或 TaskID 自相矛盾的消息会被忽略，不允许污染其他任务。
func (h *Hub) handleMessage(ctx context.Context, connected *peer, message wire.Envelope) {
	switch message.Type {
	case wire.TypePing:
		// Pong 与 TouchClient 都是尽力而为：短暂写入或数据库错误不应直接中断读循环。
		response, _ := wire.New(wire.TypePong, "", struct{}{})
		writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_ = connected.send(writeCtx, response)
		cancel()
		_ = h.store.TouchClient(ctx, connected.client.ID)
	case wire.TypeTaskAck:
		h.setTargetRunning(ctx, message.TaskID, connected.client.ID)
	case wire.TypeTaskProgress:
		// 进度频率较高且只用于实时展示，因此不写入 SQLite。
		progress, err := wire.Decode[model.Progress](message)
		if err == nil && progress.TaskID == message.TaskID {
			h.publish(message.TaskID, taskEvent{Type: "progress", Progress: &progress})
		}
	case wire.TypeTaskResult:
		result, err := wire.Decode[model.SpeedResult](message)
		if err == nil && result.TaskID == message.TaskID && h.acceptsResult(message.TaskID, connected.client.ID) {
			// ClientID 取认证连接身份而非信任 Client 载荷，防止节点冒充其他 Client。
			result.ClientID = connected.client.ID
			// 先落库再广播，保证收到 SSE result 后通过 REST 刷新可读取到相同结果。
			if err := h.store.SaveResult(ctx, result); err == nil {
				h.publish(message.TaskID, taskEvent{Type: "result", Result: &result})
			}
		}
	case wire.TypeTaskComplete:
		h.finishTarget(ctx, message.TaskID, connected.client.ID, "completed", "")
	case wire.TypeTaskFailed:
		failure, err := wire.Decode[model.Failure](message)
		if err == nil {
			h.finishTarget(ctx, message.TaskID, connected.client.ID, "failed", failure.Error)
		}
	}
}

// setTargetRunning 处理 Client 对任务分配的 ACK。
// 只有 queued -> running 是有效转换，重复 ACK、未知任务和非目标 Client 都是幂等空操作；
// 内存转换在锁内完成，持久化和 SSE 发布在锁外完成。
func (h *Hub) setTargetRunning(ctx context.Context, taskID, clientID string) {
	h.mu.Lock()
	task := h.tasks[taskID]
	transitioned := false
	if task != nil && task.targets[clientID] == "queued" {
		task.targets[clientID] = "running"
		transitioned = true
	}
	h.mu.Unlock()
	if !transitioned {
		return
	}
	_ = h.store.SetTargetStatus(ctx, taskID, clientID, "running", "")
	_ = h.store.SetTaskStatus(ctx, taskID, "running", "")
	h.publish(taskID, taskEvent{Type: "status", Status: "running"})
}

// acceptsResult 仅允许活动任务中处于 running 的目标提交结果。
// 这会拒绝未 ACK、已取消、已完成或超时后的迟到结果。
func (h *Hub) acceptsResult(taskID, clientID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	task := h.tasks[taskID]
	return task != nil && task.targets[clientID] == "running"
}

// finishTarget 将单个 Client 目标推进到给定终态，然后根据所有目标状态聚合任务。
// 对已在 completed/failed/canceled 的目标保持幂等，避免断线、超时和重复消息并发时重复
// 收敛。调用者当前只传入受控的终态字符串。
func (h *Hub) finishTarget(ctx context.Context, taskID, clientID, status, detail string) {
	h.mu.Lock()
	task := h.tasks[taskID]
	if task == nil {
		h.mu.Unlock()
		return
	}
	current := task.targets[clientID]
	if current == "completed" || current == "failed" || current == "canceled" {
		h.mu.Unlock()
		return
	}
	task.targets[clientID] = status
	h.mu.Unlock()
	_ = h.store.SetTargetStatus(ctx, taskID, clientID, status, detail)
	h.aggregate(ctx, taskID)
}

// aggregate 从数据库读取目标状态汇总；只有所有目标均为终态时才确定任务最终状态：
//   - 全部完成，任务为 completed；
//   - 全部取消，任务为 canceled；
//   - 全部失败，任务为 failed；
//   - 完成、失败或取消混合，任务为 partial。
//
// 数据库成为聚合依据，避免仅依赖进程内 map 与已持久化状态发生偏差。确定终态后停止
// 超时器、清空代理配置、删除 runtimeTask，最后向 SSE 订阅者发布状态。
func (h *Hub) aggregate(ctx context.Context, taskID string) {
	completed, failed, canceled, total, err := h.store.TargetSummary(ctx, taskID)
	if err != nil || completed+failed+canceled < total {
		return
	}
	status := "completed"
	detail := ""
	if canceled == total {
		status = "canceled"
	} else if failed == total {
		status = "failed"
		detail = "all clients failed"
	} else if failed > 0 || canceled > 0 {
		status = "partial"
		detail = "some clients did not complete"
	}
	_ = h.store.SetTaskStatus(ctx, taskID, status, detail)
	h.mu.Lock()
	if task := h.tasks[taskID]; task != nil {
		if task.expires != nil {
			task.expires.Stop()
		}
		// 主动断开对敏感代理字段的引用，使其尽早具备垃圾回收条件。
		task.assignment.Proxies = nil
		delete(h.tasks, taskID)
	}
	h.mu.Unlock()
	h.publish(taskID, taskEvent{Type: "status", Status: status, Message: detail})
}

// expireTask 是 AddTask 定时器的回调。
// 它先在读锁内快照 queued/running 目标，再逐个走 finishTarget 的正常失败路径；这样
// 数据库、聚合和 SSE 行为与 Client 主动失败保持一致，也能容忍回调与完成消息并发。
func (h *Hub) expireTask(taskID string) {
	h.mu.RLock()
	task := h.tasks[taskID]
	if task == nil {
		h.mu.RUnlock()
		return
	}
	var pending []string
	for clientID, status := range task.targets {
		if status == "queued" || status == "running" {
			pending = append(pending, clientID)
		}
	}
	h.mu.RUnlock()
	for _, clientID := range pending {
		h.finishTarget(context.Background(), taskID, clientID, "failed", "task expired after 10 minutes")
	}
}
