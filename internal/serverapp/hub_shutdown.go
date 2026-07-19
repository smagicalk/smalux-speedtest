package serverapp

import (
	"context"
	"errors"

	"github.com/coder/websocket"
)

// trackConnection 在 WebSocket Upgrade 后立即登记连接，因此尚未完成 hello
// 的连接也能被 Shutdown 打断。closing 设置后不再允许 WaitGroup.Add，保证
// Shutdown 与 WaitGroup.Wait 之间符合 sync.WaitGroup 的生命周期要求。
func (h *Hub) trackConnection(conn *websocket.Conn) bool {
	h.connectionMu.Lock()
	defer h.connectionMu.Unlock()
	if h.closing {
		return false
	}
	h.connections[conn] = struct{}{}
	h.connectionWG.Add(1)
	return true
}

// untrackConnection 在 WebSocket handler 的所有 Hub/store 清理完成后调用。
func (h *Hub) untrackConnection(conn *websocket.Conn) {
	h.connectionMu.Lock()
	delete(h.connections, conn)
	h.connectionMu.Unlock()
	h.connectionWG.Done()
}

// BeginShutdown 幂等地阻止新 WebSocket，关闭 SSE 信号和所有已升级连接。
// 它不等待 handler、不关闭 Store，因此可在 http.Server.Shutdown 之前调用。
func (h *Hub) BeginShutdown() {
	h.connectionMu.Lock()
	if h.closing {
		h.connectionMu.Unlock()
		return
	}
	h.closing = true
	connections := make([]*websocket.Conn, 0, len(h.connections))
	for connection := range h.connections {
		connections = append(connections, connection)
	}
	h.connectionMu.Unlock()
	close(h.shutdown)

	// 先从在线表隐藏全部 peer，使关闭期间的缓冲帧被 currentPeer 拒绝，
	// 也防止 unregister 为正在关闭的连接重新派发 Assignment。
	h.mu.Lock()
	h.peers = make(map[string]*peer)
	for _, task := range h.tasks {
		if task.expires != nil {
			task.expires.Stop()
		}
	}
	h.mu.Unlock()
	for _, connection := range connections {
		connection.CloseNow()
	}
}

// Done 在 Hub 开始关闭时关闭，供 SSE 等长连接立即退出。
func (h *Hub) Done() <-chan struct{} { return h.shutdown }

// Shutdown 等待全部 WebSocket handler 完成 unregister，再取消仍驻留内存的
// 任务。这个顺序确保关闭 Store 后不会有读循环或任务超时回调再次访问 SQLite。
func (h *Hub) Shutdown(ctx context.Context) error {
	h.BeginShutdown()

	done := make(chan struct{})
	go func() {
		h.connectionWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}

	h.mu.RLock()
	taskIDs := make([]string, 0, len(h.tasks))
	for taskID := range h.tasks {
		taskIDs = append(taskIDs, taskID)
	}
	h.mu.RUnlock()
	var shutdownErr error
	for _, taskID := range taskIDs {
		if err := ctx.Err(); err != nil {
			return errors.Join(shutdownErr, err)
		}
		err := h.cancelTask(ctx, taskID, "server shutting down")
		if errors.Is(err, errTaskNotActive) {
			// cancelTask 会拒绝覆盖已经线性化的超时决定。BeginShutdown 已停止所有
			// timer，因此在关库前主动补写原来的 failed 终态。
			err = h.flushPendingExpiry(ctx, taskID)
		}
		if err != nil && !errors.Is(err, errTaskNotActive) {
			shutdownErr = errors.Join(shutdownErr, err)
		}
	}
	return shutdownErr
}
