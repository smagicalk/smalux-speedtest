package serverapp

import (
	"context"
	"time"

	"smalux-speedtest/internal/logsafe"
	"smalux-speedtest/internal/model"
)

const expiredTaskDetail = "task expired after 10 minutes"

// expireTask 是 AddTask 定时器的回调。它先把超时决定记录到内存，再尝试在同一事务中
// 把所有未完成目标和父任务标记为 failed。先记录 terminalPending 很重要：即使 SQLite
// 暂时失败，Client 重连或迟到消息也不能让已超过总期限的任务重新运行。
func (h *Hub) expireTask(taskID string) {
	h.mu.RLock()
	task := h.tasks[taskID]
	h.mu.RUnlock()
	if task == nil {
		return
	}
	task.transition.Lock()
	defer task.transition.Unlock()

	h.mu.Lock()
	if h.tasks[taskID] != task {
		h.mu.Unlock()
		return
	}
	firstAttempt := !task.terminalPending
	task.terminalPending = true
	var peers []*peer
	if firstAttempt {
		for clientID, status := range task.targets {
			if (status == "assigned" || status == "running") && h.peers[clientID] != nil {
				peers = append(peers, h.peers[clientID])
			}
		}
	}
	h.mu.Unlock()

	removed, err := h.persistExpiredTask(context.Background(), taskID, task)
	if err != nil {
		h.log.Warn("persist expired task failed", "task_id", taskID, "error_type", logsafe.ErrorType(err))
		// Client 已经过了任务总期限，应立即尽力停止本地流量。数据库仍保持活动状态，
		// 因而保留不可执行的 runtimeTask，并重设一次性 timer 稍后重试原子终结。
		h.scheduleExpiryRetry(taskID, task)
		if firstAttempt {
			h.notifyTaskCancellation(context.Background(), taskID, peers)
		}
		return
	}
	if !removed {
		return
	}
	// 在释放 transition 锁前发送，保证旧 dispatch 不会排在 cancel 帧之后。
	if firstAttempt {
		h.notifyTaskCancellation(context.Background(), taskID, peers)
	}
	h.publish(taskID, taskEvent{Type: "status", Status: "failed", Message: expiredTaskDetail})
}

// persistExpiredTask 调用时必须持有 task.transition。SQLite 先提交完整终态，随后才
// 清除包含代理凭据的 runtime；返回 removed=false 表示该 runtime 已不再是当前任务。
func (h *Hub) persistExpiredTask(ctx context.Context, taskID string, task *runtimeTask) (removed bool, err error) {
	writeCtx, cancel := hubTransitionContext(ctx)
	err = h.store.FailTask(writeCtx, taskID, expiredTaskDetail)
	cancel()
	if err != nil {
		return false, err
	}
	h.mu.Lock()
	if h.tasks[taskID] == task && task.terminalPending {
		if task.expires != nil {
			task.expires.Stop()
		}
		model.EraseAssignment(&task.assignment)
		delete(h.tasks, taskID)
		removed = true
	}
	h.mu.Unlock()
	return removed, nil
}

// flushPendingExpiry 供优雅关闭使用。普通 cancelTask 不允许把已经决定的 failed 改成
// canceled，而 BeginShutdown 又会停止退避 timer，因此这里在同一个 Shutdown Context
// 内取得转换锁并补写原终态。
func (h *Hub) flushPendingExpiry(ctx context.Context, taskID string) error {
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
	pending := h.tasks[taskID] == task && task.terminalPending
	h.mu.RUnlock()
	if !pending {
		task.transition.Unlock()
		return errTaskNotActive
	}
	removed, err := h.persistExpiredTask(ctx, taskID, task)
	task.transition.Unlock()
	if err != nil {
		h.log.Warn("flush expired task failed", "task_id", taskID, "error_type", logsafe.ErrorType(err))
		return err
	}
	if removed {
		h.publish(taskID, taskEvent{Type: "status", Status: "failed", Message: expiredTaskDetail})
	}
	return nil
}

// scheduleExpiryRetry 只在任务仍是当前 runtime 且 Hub 未进入关闭流程时重设 timer。
// connectionMu -> mu 的锁顺序与 register 一致，避免重试定时器越过 BeginShutdown。
func (h *Hub) scheduleExpiryRetry(taskID string, task *runtimeTask) {
	h.connectionMu.Lock()
	defer h.connectionMu.Unlock()
	if h.closing {
		return
	}
	h.mu.Lock()
	if h.tasks[taskID] == task {
		delay := task.expiryRetryDelay
		if delay <= 0 {
			delay = taskExpiryRetryBase
		}
		next := delay * 2
		if next > taskExpiryRetryMaximum {
			next = taskExpiryRetryMaximum
		}
		task.expiryRetryDelay = next
		task.expires = time.AfterFunc(delay, func() { h.expireTask(taskID) })
	}
	h.mu.Unlock()
}
