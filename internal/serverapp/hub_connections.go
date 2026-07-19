package serverapp

import (
	"context"

	"github.com/coder/websocket"
)

// register 将新连接设为指定 Client 的唯一活动连接。
// 如果旧连接正在执行任务，相关目标先回退到 queued，以便新连接收到完整任务重新执行；
// 之后在锁外持久化状态并关闭旧连接，避免 WebSocket I/O 持有 Hub 全局锁。
func (h *Hub) register(connected *peer) {
	h.mu.Lock()
	previous := h.peers[connected.client.ID]
	var requeued []string
	if previous != nil {
		for taskID, task := range h.tasks {
			if task.targets[connected.client.ID] == "running" {
				task.targets[connected.client.ID] = "queued"
				requeued = append(requeued, taskID)
			}
		}
	}
	h.peers[connected.client.ID] = connected
	h.mu.Unlock()
	for _, taskID := range requeued {
		_ = h.store.SetTargetStatus(context.Background(), taskID, connected.client.ID, "queued", "client connection replaced")
	}
	if previous != nil {
		previous.conn.Close(websocket.StatusPolicyViolation, "replaced by a new connection")
	}
}

// RevokeClient 从在线表移除连接、关闭 WebSocket，并把该 Client 的 queued/running 目标
// 标为 failed。数据库凭据禁用由调用方先完成，本方法负责 Hub 运行态的即时收敛。
func (h *Hub) RevokeClient(ctx context.Context, clientID string) {
	h.mu.Lock()
	connected := h.peers[clientID]
	if connected != nil {
		delete(h.peers, clientID)
	}
	var activeTasks []string
	for taskID, task := range h.tasks {
		status := task.targets[clientID]
		if status == "queued" || status == "running" {
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
// running 目标回退 queued，允许同一 Client 使用有效 Token 重连后从头领取任务。
func (h *Hub) unregister(connected *peer) {
	h.mu.Lock()
	var requeued []string
	if h.peers[connected.client.ID] == connected {
		delete(h.peers, connected.client.ID)
		for taskID, task := range h.tasks {
			if task.targets[connected.client.ID] == "running" {
				task.targets[connected.client.ID] = "queued"
				requeued = append(requeued, taskID)
			}
		}
	}
	h.mu.Unlock()
	for _, taskID := range requeued {
		_ = h.store.SetTargetStatus(context.Background(), taskID, connected.client.ID, "queued", "client disconnected")
	}
	// 请求 Context 已随 WebSocket 结束，清理性数据库操作使用独立 Background Context。
	_ = h.store.TouchClient(context.Background(), connected.client.ID)
	h.log.Info("client disconnected", "client_id", connected.client.ID)
}
