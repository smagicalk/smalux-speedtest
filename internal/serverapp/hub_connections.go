package serverapp

import (
	"context"

	"github.com/coder/websocket"

	"smalux-speedtest/internal/logsafe"
)

// register 将新连接设为指定 Client 的唯一活动连接。
// 如果旧连接正在执行任务，相关目标先回退到 queued，以便新连接收到完整任务重新执行；
// 之后在锁外持久化状态并关闭旧连接，避免 WebSocket I/O 持有 Hub 全局锁。
func (h *Hub) register(connected *peer) bool {
	h.connectionMu.Lock()
	if h.closing {
		h.connectionMu.Unlock()
		return false
	}
	h.mu.Lock()
	if _, revoked := h.revokedClients[connected.client.ID]; revoked {
		h.mu.Unlock()
		h.connectionMu.Unlock()
		return false
	}
	previous := h.peers[connected.client.ID]
	var requeued []string
	if previous != nil {
		for taskID, task := range h.tasks {
			if !task.terminalPending && targetNeedsRequeue(task.targets[connected.client.ID]) {
				requeued = append(requeued, taskID)
			}
		}
	}
	h.peers[connected.client.ID] = connected
	h.mu.Unlock()
	// 只需用 connectionMu 保证“检查 closing + 登记 peer”不与 Shutdown
	// 交错；后续 SQLite 和 WebSocket I/O 必须在锁外执行。
	h.connectionMu.Unlock()
	for _, taskID := range requeued {
		h.requeueTarget(context.Background(), taskID, connected.client.ID, "client connection replaced")
	}
	if previous != nil {
		previous.conn.Close(websocket.StatusPolicyViolation, "replaced by a new connection")
	}
	return true
}

// RevokeClient 记录永久撤销标记、从在线表移除连接、关闭 WebSocket，并把该 Client
// 的未完成目标标为 failed。数据库凭据必须由调用方先禁用；同一把 Hub 锁保证尚在
// hello 阶段的连接之后无法重新登记。
func (h *Hub) RevokeClient(ctx context.Context, clientID string) {
	h.mu.Lock()
	h.revokedClients[clientID] = struct{}{}
	connected := h.peers[clientID]
	if connected != nil {
		delete(h.peers, clientID)
	}
	var activeTasks []string
	for taskID, task := range h.tasks {
		status := task.targets[clientID]
		if status == "queued" || status == "assigned" || status == "running" {
			activeTasks = append(activeTasks, taskID)
		}
	}
	h.mu.Unlock()
	if connected != nil {
		connected.conn.Close(websocket.StatusPolicyViolation, "client token revoked")
	}
	for _, taskID := range activeTasks {
		h.finishTarget(ctx, taskID, clientID, "failed", "client token revoked")
	}
}

// unregister 清理一个已经结束的连接。
// 只有该 peer 仍是 peers 中的当前连接时才删除，防止被替换的旧连接退出后误伤新连接。
// assigned/running 目标回退 queued，允许同一 Client 使用有效 Token 重连后从头领取任务。
func (h *Hub) unregister(connected *peer) {
	h.mu.Lock()
	var requeued []string
	if h.peers[connected.client.ID] == connected {
		delete(h.peers, connected.client.ID)
		for taskID, task := range h.tasks {
			if !task.terminalPending && targetNeedsRequeue(task.targets[connected.client.ID]) {
				requeued = append(requeued, taskID)
			}
		}
	}
	h.mu.Unlock()
	for _, taskID := range requeued {
		h.requeueTarget(context.Background(), taskID, connected.client.ID, "client disconnected")
	}
	// 请求 Context 已随 WebSocket 结束，清理性数据库操作使用独立 Background Context。
	_ = h.store.TouchClient(context.Background(), connected.client.ID)
	h.log.Info("client disconnected", "client_id", connected.client.ID)
}

// requeueTarget 在任务级转换锁内把 assigned/running 退回 queued。running
// 需要同步更新 SQLite，assigned 在数据库中原本就是 queued。更新后会立即尝试向
// 此刻的新连接重发，覆盖“旧连接删除后、重排队前”发生快速重连的交错。
func (h *Hub) requeueTarget(ctx context.Context, taskID, clientID, detail string) {
	h.mu.RLock()
	task := h.tasks[taskID]
	h.mu.RUnlock()
	if task == nil {
		return
	}
	task.transition.Lock()
	h.mu.RLock()
	current := task.targets[clientID]
	active := h.tasks[taskID] == task && !task.terminalPending && targetNeedsRequeue(current)
	h.mu.RUnlock()
	if !active {
		task.transition.Unlock()
		return
	}
	changed := true
	if current == "running" {
		writeCtx, cancel := hubTransitionContext(ctx)
		var err error
		changed, err = h.store.RequeueTarget(writeCtx, taskID, clientID, detail)
		cancel()
		if err != nil {
			task.transition.Unlock()
			h.log.Warn("persist target requeue failed", "task_id", taskID, "client_id", clientID, "error_type", logsafe.ErrorType(err))
			return
		}
	}
	if !changed {
		task.transition.Unlock()
		return
	}
	h.mu.Lock()
	if h.tasks[taskID] == task && task.targets[clientID] == current {
		task.targets[clientID] = "queued"
	}
	h.mu.Unlock()
	task.transition.Unlock()
	// 断线窗口内已有新 peer 登记时立即补发；没有在线 peer 时 dispatch 幂等返回。
	h.dispatch(taskID, clientID)
}

func targetNeedsRequeue(status string) bool { return status == "assigned" || status == "running" }
