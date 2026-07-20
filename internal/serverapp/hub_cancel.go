package serverapp

import (
	"context"
	"errors"

	"smalux-speedtest/internal/logsafe"
	"smalux-speedtest/internal/model"
	"smalux-speedtest/internal/wire"
)

var errTaskNotActive = errors.New("task is not active")

// CancelTask 将一个活动任务及所有未完成目标标记为 canceled。
//
// 同一任务的转换锁会排斥 ACK、断线重排队和终结消息。SQLite 先在一个事务中持久化父
// 任务及所有未完成目标，成功后才删除运行态并通知 Client，避免取消只存在于内存。
func (h *Hub) CancelTask(ctx context.Context, taskID string) error {
	return h.cancelTask(ctx, taskID, "canceled by administrator")
}

func (h *Hub) cancelTask(ctx context.Context, taskID, detail string) error {
	h.mu.RLock()
	task := h.tasks[taskID]
	h.mu.RUnlock()
	if task == nil {
		return errTaskNotActive
	}
	if err := lockTaskTransition(ctx, &task.transition); err != nil {
		return err
	}
	h.mu.RLock()
	// 超时决定在第一次 FailTask 前已线性化。普通取消不能在数据库重试窗口内把
	// 既定 failed 终态改写为 canceled；Shutdown 会通过 flushPendingExpiry 补写失败。
	active := h.tasks[taskID] == task && !task.terminalPending
	h.mu.RUnlock()
	if !active {
		task.transition.Unlock()
		return errTaskNotActive
	}
	var peers []*peer
	h.mu.RLock()
	for clientID, status := range task.targets {
		if status == "queued" || status == "assigned" || status == "running" {
			if connected := h.peers[clientID]; connected != nil {
				peers = append(peers, connected)
			}
		}
	}
	h.mu.RUnlock()
	writeCtx, writeCancel := hubTransitionContext(ctx)
	err := h.store.CancelTask(writeCtx, taskID, detail)
	writeCancel()
	if err != nil {
		task.transition.Unlock()
		h.log.Warn("persist task cancellation failed", "task_id", taskID, "error_type", logsafe.ErrorType(err))
		return err
	}
	h.mu.Lock()
	if h.tasks[taskID] == task {
		if task.expires != nil {
			task.expires.Stop()
		}
		for clientID, status := range task.targets {
			if status == "queued" || status == "assigned" || status == "running" {
				task.targets[clientID] = "canceled"
			}
		}
		model.EraseAssignment(&task.assignment)
		delete(h.tasks, taskID)
	}
	h.mu.Unlock()
	task.transition.Unlock()

	h.notifyTaskCancellation(ctx, taskID, peers)
	h.publish(taskID, taskEvent{Type: "status", Status: "canceled"})
	return nil
}

// notifyTaskCancellation 向一组已取得任务的 peer 发送停止帧。取消和超时失败
// 共用该协议消息；任务的最终 canceled/failed 状态仍以 SQLite/SSE 为准。
func (h *Hub) notifyTaskCancellation(ctx context.Context, taskID string, peers []*peer) {
	message, _ := wire.New(wire.TypeTaskCancel, taskID, model.Ack{TaskID: taskID})
	// 全部 Client 共用一个 5 秒窗口，避免多节点时取消请求线性阻塞。
	sendCtx, sendCancel := hubTransitionContext(ctx)
	defer sendCancel()
	for _, connected := range peers {
		// 数据库已提交后仍尝试发出取消帧，不让已断开的 HTTP 请求 Context
		// 使 Client 继续消耗流量。单个失败不影响数据库终态或其他 Client。
		_ = connected.send(sendCtx, message)
	}
}
