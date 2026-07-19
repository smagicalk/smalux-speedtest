package serverapp

import (
	"context"
	"sync"
	"time"

	"smalux-speedtest/internal/model"
	"smalux-speedtest/internal/wire"
)

const (
	taskLifetime           = 10 * time.Minute
	taskExpiryRetryBase    = 5 * time.Second
	taskExpiryRetryMaximum = time.Minute
)

// runtimeTask 是一个尚未收敛到终态的任务运行时记录。
//
// assignment.Proxies 包含下发给 Client 所需的完整 sing-box outbound，其中可能存在
// 密码、UUID、私钥等敏感信息。该结构只驻留内存，不经 store 持久化；任务结束时会
// 清空代理切片并从 Hub 删除。
type runtimeTask struct {
	// transition 串行化同一任务的 ACK、重排队、终结和取消事务，但不阻塞其他任务。
	transition sync.Mutex
	// assignment 是发往每个目标 Client 的不可变任务内容。
	assignment model.Assignment
	// targets 记录每个目标的状态，主路径为 queued -> assigned -> running ->
	// completed/failed。assigned 是防止重复下发的纯内存状态，SQLite 中仍表示为 queued；
	// 管理员取消可进入 canceled，assigned/running Client 断线或被替换时会回到 queued。
	targets map[string]string
	// resultProxies 是从 Assignment 派生的只读身份表。Client 结果只能引用其中的 ID，
	// 代理名称、协议和脱敏地址也始终由此服务端快照覆盖。
	resultProxies map[string]resultProxyIdentity
	// resultKeys 按 Client 记录已成功落库的“代理 + 测速节点”键。它只在 transition
	// 锁内访问，用于限制异常 Client 构造无限唯一结果行。
	resultKeys map[string]map[string]struct{}
	// resultCounts 进一步按 Client 和代理统计唯一键，防止一个代理占满整批总配额。
	resultCounts map[string]map[string]int
	// resultLimit 是每个目标理论上最多返回的结果数：代理数 × max(TopN, 1)。
	resultLimit int
	// resultLimitPerProxy 是单个代理允许的 Speedtest 节点结果上限。
	resultLimitPerProxy int
	// expires 为任务设置最长 10 分钟运行窗口，终态或取消时必须停止。
	expires *time.Timer
	// expiryRetryDelay 在超时落库失败时按 5s、10s...1min 退避，避免数据库长期故障
	// 期间以固定高频率重试和重复发送停止帧。
	expiryRetryDelay time.Duration
	// terminalPending 表示任务已超过总期限，但第一次终态事务因临时数据库错误尚未
	// 提交。它一经设置便不再接受 ACK、结果、完成、重排队或新 Assignment；定时器只
	// 重试持久化同一个 failed 终态，避免 Client 重连重新消耗流量。
	terminalPending bool
}

// AddTask 注册完整任务 Assignment、初始化所有目标为 queued，并立即尝试向在线目标派发。
// 离线目标保留 queued，之后在 Client 完成 WebSocket 握手时由 dispatchQueued 补发。
func (h *Hub) AddTask(assignment model.Assignment, clientIDs []string) {
	resultLimitPerProxy := assignment.TopN
	if resultLimitPerProxy < 1 {
		resultLimitPerProxy = 1
	}
	task := &runtimeTask{
		assignment: assignment, targets: make(map[string]string),
		resultProxies:       make(map[string]resultProxyIdentity, len(assignment.Proxies)),
		resultKeys:          make(map[string]map[string]struct{}, len(clientIDs)),
		resultCounts:        make(map[string]map[string]int, len(clientIDs)),
		resultLimit:         len(assignment.Proxies) * resultLimitPerProxy,
		resultLimitPerProxy: resultLimitPerProxy,
	}
	for _, proxy := range assignment.Proxies {
		task.resultProxies[proxy.ID] = resultIdentityFromProxy(proxy)
	}
	for _, id := range clientIDs {
		task.targets[id] = "queued"
	}
	// 超时回调可能与 ACK、完成或管理员取消并发；后续转换函数会在锁内检查当前状态，
	// 使重复终态通知保持幂等。
	task.expires = time.AfterFunc(taskLifetime, func() { h.expireTask(assignment.TaskID) })
	h.mu.Lock()
	h.tasks[assignment.TaskID] = task
	var revoked []string
	var dispatchable []string
	for _, id := range clientIDs {
		if _, disabled := h.revokedClients[id]; disabled {
			revoked = append(revoked, id)
		} else {
			dispatchable = append(dispatchable, id)
		}
	}
	h.mu.Unlock()
	// CreateTask 提交后到 AddTask 之间若发生撤销，RevokeClient 可能已经扫描完运行态。
	// revokedClients 在同一 Hub 锁下补上该窗口，并将持久化目标立即收敛为 failed。
	for _, id := range revoked {
		h.finishTarget(context.Background(), assignment.TaskID, id, "failed", "client token revoked")
	}
	for _, id := range dispatchable {
		h.dispatch(assignment.TaskID, id)
	}
}

// dispatchQueued 收集指定 Client 的 queued 任务，并在释放读锁后逐个尝试派发。
func (h *Hub) dispatchQueued(clientID string) {
	h.mu.RLock()
	var taskIDs []string
	for taskID, task := range h.tasks {
		if !task.terminalPending && task.targets[clientID] == "queued" {
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
// 发送前会进入纯内存 assigned 状态，只有 Client 返回 task.ack 后才持久化为
// running。连接在收到完整任务前断开时仍可在重连后再次派发。发送期间
// 持有该任务的转换锁，保证
// 取消帧不会先于旧 Assignment 帧发出；Hub 全局锁仍在网络 I/O 前释放。
func (h *Hub) dispatch(taskID, clientID string) {
	h.mu.RLock()
	task := h.tasks[taskID]
	h.mu.RUnlock()
	if task == nil {
		return
	}
	task.transition.Lock()
	defer task.transition.Unlock()
	h.mu.Lock()
	connected := h.peers[clientID]
	if h.tasks[taskID] != task || task.terminalPending || connected == nil || task.targets[clientID] != "queued" {
		h.mu.Unlock()
		return
	}
	assignment := task.assignment
	// assigned 预留发送权，并发 dispatchQueued/AddTask 会在上方检查中退出。
	// 它不写入 SQLite，只有 Client ACK 后才持久化为 running。
	task.targets[clientID] = "assigned"
	h.mu.Unlock()
	message, err := wire.New(wire.TypeTaskAssign, taskID, assignment)
	if err != nil {
		h.mu.Lock()
		if h.tasks[taskID] == task && task.targets[clientID] == "assigned" {
			task.targets[clientID] = "queued"
		}
		h.mu.Unlock()
		h.log.Warn("encode task assignment failed", "task_id", taskID, "client_id", clientID, "error", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	err = connected.send(ctx, message)
	cancel()
	if err != nil {
		h.mu.Lock()
		if h.tasks[taskID] == task && task.targets[clientID] == "assigned" {
			task.targets[clientID] = "queued"
		}
		h.mu.Unlock()
		h.log.Warn("task dispatch failed", "task_id", taskID, "client_id", clientID, "error", err)
	}
}
