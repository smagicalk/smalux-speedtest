package serverapp

import (
	"context"
	"time"

	"smalux-speedtest/internal/logsafe"
	"smalux-speedtest/internal/model"
	"smalux-speedtest/internal/wire"
)

// handleMessage 按 wire.Type 驱动任务状态机。
//
// 心跳只刷新 Client 活跃时间；ACK 将已派发目标转为 running；进度只广播不持久化；结果在
// 确认目标仍为 running 后持久化；complete/failed 则完成单个目标并触发任务聚合。无法
// 解码或 TaskID 自相矛盾的消息会被忽略，不允许污染其他任务。
func (h *Hub) handleMessage(ctx context.Context, connected *peer, message wire.Envelope) {
	// 同一 Client 重连后，旧 WebSocket 的读循环可能还有已缓冲帧。仅当前 peer
	// 可以驱动状态机，避免旧 ACK/complete 污染已重排队的任务。
	if !h.currentPeer(connected) {
		return
	}
	// 当前任务 ID 由服务端生成 32 字节十六进制文本；提前拒绝异常长键，避免在 Hub
	// map 和日志字段中反复散列或保留接近 WebSocket 上限的攻击性字符串。
	if len(message.TaskID) > 128 {
		return
	}
	switch message.Type {
	case wire.TypePing:
		// Pong 与 TouchClient 都是尽力而为：短暂写入或数据库错误不应直接中断读循环。
		response, _ := wire.New(wire.TypePong, "", struct{}{})
		writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_ = connected.send(writeCtx, response)
		cancel()
		_ = h.store.TouchClient(ctx, connected.client.ID)
	case wire.TypeTaskAck:
		ack, err := wire.Decode[model.Ack](message)
		if err == nil && ack.TaskID == message.TaskID {
			h.setTargetRunning(ctx, message.TaskID, connected)
		}
	case wire.TypeTaskProgress:
		// 进度频率较高且只用于实时展示，因此不写入 SQLite；所有展示字段仍需由
		// Assignment 快照覆盖，防止恶意 Client 经 SSE 夹带节点配置。
		progress, err := wire.Decode[model.Progress](message)
		if err == nil && progress.TaskID == message.TaskID {
			if normalized, ok := h.normalizeProgress(message.TaskID, connected, progress); ok {
				h.publish(message.TaskID, taskEvent{Type: "progress", Progress: &normalized})
			}
		}
	case wire.TypeTaskResult:
		result, err := wire.Decode[model.SpeedResult](message)
		if err == nil && result.TaskID == message.TaskID {
			// ClientID 取认证连接身份而非信任 Client 载荷，防止节点冒充其他 Client。
			result.ClientID = connected.client.ID
			h.saveResult(ctx, connected, result)
		}
	case wire.TypeTaskComplete:
		complete, err := wire.Decode[model.Ack](message)
		if err == nil && complete.TaskID == message.TaskID {
			h.finishTargetFromPeer(ctx, message.TaskID, connected, "completed", "")
		}
	case wire.TypeTaskFailed:
		failure, err := wire.Decode[model.Failure](message)
		if err == nil && failure.TaskID == message.TaskID {
			// Failure.Error 同样来自远程 Client，只接受固定任务失败类别。
			h.finishTargetFromPeer(ctx, message.TaskID, connected, "failed", model.NormalizeTaskFailure(failure.Error))
		}
	}
}

// setTargetRunning 处理 Client 对任务分配的 ACK。
// 只有 assigned -> running 是有效内存转换（SQLite 中 assigned 仍为 queued）；
// 重复 ACK、未知任务、非目标 Client 和被替换的旧连接都是幂等空操作。
// SQLite 转换在任务锁内先提交，内存快照随后更新，避免两者分叉。
func (h *Hub) setTargetRunning(ctx context.Context, taskID string, connected *peer) {
	clientID := connected.client.ID
	h.mu.RLock()
	task := h.tasks[taskID]
	h.mu.RUnlock()
	if task == nil {
		return
	}
	task.transition.Lock()
	defer task.transition.Unlock()
	h.mu.RLock()
	active := h.tasks[taskID] == task && !task.terminalPending && h.peers[clientID] == connected && task.targets[clientID] == "assigned"
	h.mu.RUnlock()
	if !active {
		return
	}
	writeCtx, cancel := hubTransitionContext(ctx)
	transitioned, err := h.store.StartTarget(writeCtx, taskID, clientID)
	cancel()
	if err != nil {
		h.log.Warn("persist running target failed", "task_id", taskID, "client_id", clientID, "error_type", logsafe.ErrorType(err))
		return
	}
	if !transitioned {
		return
	}
	h.mu.Lock()
	// StartTarget 已提交后必须同步更新内存。peer 可能在 SQLite 写入期间被
	// 替换/移除，相应 register/unregister 已快照 assigned 并正等待同一把
	// transition 锁；它会在此处解锁后把 running 的数据库和内存一起退回 queued。
	if h.tasks[taskID] != task || task.targets[clientID] != "assigned" {
		h.mu.Unlock()
		return
	}
	task.targets[clientID] = "running"
	h.mu.Unlock()
	h.publish(taskID, taskEvent{Type: "status", Status: "running"})
}

// currentPeer 在消息解码前拒绝已被替换连接的缓冲帧。
func (h *Hub) currentPeer(connected *peer) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return connected != nil && h.peers[connected.client.ID] == connected
}
