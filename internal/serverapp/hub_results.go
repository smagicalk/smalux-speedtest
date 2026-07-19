package serverapp

import (
	"context"

	"smalux-speedtest/internal/model"
)

// saveResult 只持久化活动任务中 running 目标的结果。
//
// 校验、SQLite 写入和 SSE 广播共用任务转换锁，因此取消或断线重排队不能插入
// “内存还是 running、数据库已是 canceled/queued”的间隙。如果结果先取得锁，
// 它作为取消前已到达的测量保留；取消先取得锁时，迟到结果会被拒绝。
func (h *Hub) saveResult(ctx context.Context, connected *peer, result model.SpeedResult) {
	h.mu.RLock()
	task := h.tasks[result.TaskID]
	h.mu.RUnlock()
	if task == nil {
		return
	}
	task.transition.Lock()
	defer task.transition.Unlock()
	h.mu.RLock()
	active := h.tasks[result.TaskID] == task && !task.terminalPending && h.peers[result.ClientID] == connected && task.targets[result.ClientID] == "running"
	h.mu.RUnlock()
	if !active {
		return
	}
	validated, resultKey, ok := task.validateResult(result.ClientID, result)
	if !ok {
		h.log.Warn("rejected invalid task result", "task_id", result.TaskID, "client_id", result.ClientID)
		return
	}
	writeCtx, cancel := hubTransitionContext(ctx)
	err := h.store.SaveResult(writeCtx, validated)
	cancel()
	if err != nil {
		h.log.Warn("persist task result failed", "task_id", result.TaskID, "client_id", result.ClientID, "error", err)
		return
	}
	task.recordResultKey(result.ClientID, resultKey)
	// 先落库再广播，保证收到 SSE result 后通过 REST 刷新可读取到相同结果。
	h.publish(result.TaskID, taskEvent{Type: "result", Result: &validated})
}

// finishTarget 将单个 Client 目标推进到给定终态，然后根据所有目标状态聚合任务。
// 对已在 completed/failed/canceled 的目标保持幂等，避免断线、超时和重复消息并发时重复
// 收敛。调用者当前只传入受控的终态字符串。
func (h *Hub) finishTarget(ctx context.Context, taskID, clientID, status, detail string) {
	h.finishTargetLocked(ctx, taskID, clientID, nil, status, detail)
}

// finishTargetFromPeer 额外要求消息来自该 Client 的当前连接，且目标已通过
// ACK 进入 running。内部超时/撤销路径仍可以通过 finishTarget 终结未 ACK 目标。
func (h *Hub) finishTargetFromPeer(ctx context.Context, taskID string, connected *peer, status, detail string) {
	h.finishTargetLocked(ctx, taskID, connected.client.ID, connected, status, detail)
}

func (h *Hub) finishTargetLocked(ctx context.Context, taskID, clientID string, expected *peer, status, detail string) {
	h.mu.RLock()
	task := h.tasks[taskID]
	h.mu.RUnlock()
	if task == nil {
		return
	}
	task.transition.Lock()
	defer task.transition.Unlock()
	h.mu.RLock()
	active := h.tasks[taskID] == task && !task.terminalPending
	current := task.targets[clientID]
	if expected != nil {
		active = active && h.peers[clientID] == expected && current == "running"
	}
	h.mu.RUnlock()
	if !active || (expected == nil && current != "queued" && current != "assigned" && current != "running") {
		return
	}
	writeCtx, cancel := hubTransitionContext(ctx)
	transition, err := h.store.FinishTarget(writeCtx, taskID, clientID, status, detail)
	cancel()
	if err != nil {
		h.log.Warn("persist terminal target failed", "task_id", taskID, "client_id", clientID, "status", status, "error", err)
		return
	}
	removed := false
	h.mu.Lock()
	if h.tasks[taskID] == task {
		task.targets[clientID] = transition.TargetStatus
		if transition.TaskTerminal {
			if task.expires != nil {
				task.expires.Stop()
			}
			// 主动断开对敏感代理字段的引用，使其尽早具备垃圾回收条件。
			task.assignment.Proxies = nil
			delete(h.tasks, taskID)
			removed = true
		}
	}
	h.mu.Unlock()
	if removed {
		h.publish(taskID, taskEvent{Type: "status", Status: transition.TaskStatus, Message: transition.Detail})
	}
}
