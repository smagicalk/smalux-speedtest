package serverapp

import (
	"context"
	"errors"
	"time"

	"smalux-speedtest/internal/model"
	"smalux-speedtest/internal/wire"
)

// runtimeTask 是一个尚未收敛到终态的任务运行时记录。
//
// assignment.Proxies 包含下发给 Client 所需的完整 sing-box outbound，其中可能存在
// 密码、UUID、私钥等敏感信息。该结构只驻留内存，不经 store 持久化；任务结束时会
// 清空代理切片并从 Hub 删除。
type runtimeTask struct {
	// assignment 是发往每个目标 Client 的不可变任务内容。
	assignment model.Assignment
	// targets 记录每个目标的状态，合法主路径为 queued -> running -> completed/failed，
	// 管理员取消可进入 canceled；running Client 断线或连接被替换时会回到 queued。
	targets map[string]string
	// expires 为任务设置最长 10 分钟运行窗口，终态或取消时必须停止。
	expires *time.Timer
}

// AddTask 注册完整任务 Assignment、初始化所有目标为 queued，并立即尝试向在线目标派发。
// 离线目标保留 queued，之后在 Client 完成 WebSocket 握手时由 dispatchQueued 补发。
func (h *Hub) AddTask(assignment model.Assignment, clientIDs []string) {
	task := &runtimeTask{assignment: assignment, targets: make(map[string]string)}
	for _, id := range clientIDs {
		task.targets[id] = "queued"
	}
	// 超时回调可能与 ACK、完成或管理员取消并发；后续转换函数会在锁内检查当前状态，
	// 使重复终态通知保持幂等。
	task.expires = time.AfterFunc(10*time.Minute, func() { h.expireTask(assignment.TaskID) })
	h.mu.Lock()
	h.tasks[assignment.TaskID] = task
	h.mu.Unlock()
	for _, id := range clientIDs {
		h.dispatch(assignment.TaskID, id)
	}
}

// CancelTask 将一个活动任务及所有未完成目标标记为 canceled。
//
// 状态和待通知 peer 在持有 Hub 锁时形成快照，网络写入和数据库更新在锁外进行。任务会
// 立即从运行态 map 删除，释放包含代理凭据的 Assignment；已完成/失败的目标不再发送取消。
func (h *Hub) CancelTask(ctx context.Context, taskID string) error {
	h.mu.Lock()
	task, ok := h.tasks[taskID]
	if !ok {
		h.mu.Unlock()
		return errors.New("task is not active")
	}
	if task.expires != nil {
		task.expires.Stop()
	}
	var peers []*peer
	for clientID, status := range task.targets {
		if status != "completed" && status != "failed" {
			task.targets[clientID] = "canceled"
			if connected := h.peers[clientID]; connected != nil {
				peers = append(peers, connected)
			}
		}
	}
	delete(h.tasks, taskID)
	h.mu.Unlock()

	message, _ := wire.New(wire.TypeTaskCancel, taskID, model.Ack{TaskID: taskID})
	for _, connected := range peers {
		// 单个 Client 最多占用 5 秒，发送失败不阻止其余 Client 和数据库状态收敛。
		sendCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_ = connected.send(sendCtx, message)
		cancel()
	}
	_ = h.store.SetTaskStatus(ctx, taskID, "canceled", "canceled by administrator")
	h.publish(taskID, taskEvent{Type: "status", Status: "canceled"})
	return nil
}

// dispatchQueued 收集指定 Client 的 queued 任务，并在释放读锁后逐个尝试派发。
func (h *Hub) dispatchQueued(clientID string) {
	h.mu.RLock()
	var taskIDs []string
	for taskID, task := range h.tasks {
		if task.targets[clientID] == "queued" {
			taskIDs = append(taskIDs, taskID)
		}
	}
	h.mu.RUnlock()
	for _, taskID := range taskIDs {
		h.dispatch(taskID, clientID)
	}
}

// dispatch 向一个当前在线且状态仍为 queued 的目标发送 Assignment。
//
// 发送成功本身不改变状态；只有 Client 返回 task.ack 后才进入 running。这样连接在收到
// 完整任务前断开时仍可在重连后再次派发。复制 assignment 后立即释放读锁，网络阻塞不会
// 阻塞 Hub 的其他状态转换。
func (h *Hub) dispatch(taskID, clientID string) {
	h.mu.RLock()
	task := h.tasks[taskID]
	connected := h.peers[clientID]
	if task == nil || connected == nil || task.targets[clientID] != "queued" {
		h.mu.RUnlock()
		return
	}
	assignment := task.assignment
	h.mu.RUnlock()
	message, err := wire.New(wire.TypeTaskAssign, taskID, assignment)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	err = connected.send(ctx, message)
	cancel()
	if err != nil {
		h.log.Warn("task dispatch failed", "task_id", taskID, "client_id", clientID, "error", err)
	}
}
