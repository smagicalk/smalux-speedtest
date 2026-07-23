package serverapp

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"sync"
	"time"

	"smalux-speedtest/internal/logsafe"
	"smalux-speedtest/internal/model"
	"smalux-speedtest/internal/wire"
)

type workRef struct {
	taskID   string
	clientID string
	proxyID  string
	workID   string
}

type workLease struct {
	ref      workRef
	proxyKey [32]byte
	state    string
}

type targetWork struct {
	order     []string
	completed map[string]struct{}
	active    *workLease
}

type reservedWork struct {
	task       *runtimeTask
	connected  *peer
	assignment model.Assignment
	ref        workRef
}

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
	// work tracks per-Client proxy completion while target status remains the
	// persisted aggregate. proxyKeys are ephemeral HMAC values used only to prevent
	// two Clients from testing the same proxy at once.
	work        map[string]*targetWork
	proxyKeys   map[string][32]byte
	proxyIndex  map[string]int
	targetOrder []string
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
	// AddTask is also used by tests and future non-HTTP integrations, so do not
	// assume importer.Parse was the only producer. Copy the slice before applying
	// the shared name rule; the caller's assignment remains its own snapshot.
	assignment.Proxies = append([]model.ProxySpec(nil), assignment.Proxies...)
	for index := range assignment.Proxies {
		proxy := &assignment.Proxies[index]
		proxy.Protocol = model.NormalizeResultProtocol(proxy.Protocol)
		proxy.Name = model.NormalizeProxyName(proxy.Protocol, proxy.Name, proxy.Server)
	}
	resultLimitPerProxy := assignment.TopN
	if resultLimitPerProxy < 1 {
		resultLimitPerProxy = 1
	}
	task := &runtimeTask{
		assignment: assignment, targets: make(map[string]string),
		work:                make(map[string]*targetWork, len(clientIDs)),
		proxyKeys:           make(map[string][32]byte, len(assignment.Proxies)),
		proxyIndex:          make(map[string]int, len(assignment.Proxies)),
		targetOrder:         append([]string(nil), clientIDs...),
		resultProxies:       make(map[string]resultProxyIdentity, len(assignment.Proxies)),
		resultKeys:          make(map[string]map[string]struct{}, len(clientIDs)),
		resultCounts:        make(map[string]map[string]int, len(clientIDs)),
		resultLimit:         len(assignment.Proxies) * resultLimitPerProxy,
		resultLimitPerProxy: resultLimitPerProxy,
	}
	proxyIDs := make([]string, 0, len(assignment.Proxies))
	for index, proxy := range assignment.Proxies {
		task.resultProxies[proxy.ID] = resultIdentityFromProxy(proxy)
		task.proxyKeys[proxy.ID] = h.proxyWorkKey(proxy.Outbound)
		task.proxyIndex[proxy.ID] = index
		proxyIDs = append(proxyIDs, proxy.ID)
	}
	// HTTP task creation rejects an empty import. Keep AddTask defensive because it
	// is also used by tests and may later be called by non-HTTP integrations.
	if len(proxyIDs) == 0 {
		return
	}
	for clientIndex, id := range clientIDs {
		task.targets[id] = "queued"
		order := make([]string, len(proxyIDs))
		for index := range proxyIDs {
			order[index] = proxyIDs[(index+clientIndex)%len(proxyIDs)]
		}
		task.work[id] = &targetWork{order: order, completed: make(map[string]struct{}, len(proxyIDs))}
	}
	// 超时回调可能与 ACK、完成或管理员取消并发；后续转换函数会在锁内检查当前状态，
	// 使重复终态通知保持幂等。
	task.expires = time.AfterFunc(taskLifetime, func() { h.expireTask(assignment.TaskID) })
	h.mu.Lock()
	h.tasks[assignment.TaskID] = task
	h.taskOrder = append(h.taskOrder, assignment.TaskID)
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
	if len(dispatchable) > 0 {
		h.schedule()
	}
}

// dispatchQueued and dispatch remain narrow compatibility points for connection and
// lifecycle code. The scheduler always considers every idle Client and active task so
// freed capacity can be used immediately by another task.
func (h *Hub) dispatchQueued(string)   { h.schedule() }
func (h *Hub) dispatch(string, string) { h.schedule() }

// schedule fills all currently usable Client capacity. Each reservation owns both a
// Client slot and an ephemeral proxy fingerprint, so no Client receives a second unit
// and no two Clients test the same proxy concurrently.
func (h *Hub) schedule() {
	h.scheduleMu.Lock()
	defer h.scheduleMu.Unlock()
	excluded := make(map[string]bool)
	for {
		reserved := h.reserveNextWork(excluded)
		if reserved == nil {
			return
		}
		reserved.task.transition.Lock()
		if !h.reservationActive(reserved) {
			reserved.task.transition.Unlock()
			continue
		}
		message, err := wire.New(wire.TypeTaskAssign, reserved.ref.taskID, reserved.assignment)
		if err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err = reserved.connected.send(ctx, message)
			cancel()
		}
		if err != nil {
			h.rollbackReservedWork(reserved)
			excluded[reserved.ref.clientID] = true
			reserved.connected.conn.CloseNow()
			h.log.Warn("task work dispatch failed", "task_id", reserved.ref.taskID, "client_id", reserved.ref.clientID, "error_type", logsafe.ErrorType(err))
		}
		reserved.task.transition.Unlock()
	}
}

func (h *Hub) reserveNextWork(excluded map[string]bool) *reservedWork {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.taskOrder) == 0 {
		return nil
	}
	for offset := 0; offset < len(h.taskOrder); offset++ {
		taskPosition := (h.scheduleNext + offset) % len(h.taskOrder)
		taskID := h.taskOrder[taskPosition]
		task := h.tasks[taskID]
		if task == nil || task.terminalPending {
			continue
		}
		for _, clientID := range task.targetOrder {
			status := task.targets[clientID]
			target := task.work[clientID]
			connected := h.peers[clientID]
			if excluded[clientID] || connected == nil || target == nil || target.active != nil ||
				(status != "queued" && status != "running") {
				continue
			}
			if _, busy := h.clientWork[clientID]; busy {
				continue
			}
			proxyID, proxyKey, ok := h.nextUnlockedProxy(task, target)
			if !ok {
				continue
			}
			workID := model.NewID()
			ref := workRef{taskID: taskID, clientID: clientID, proxyID: proxyID, workID: workID}
			target.active = &workLease{ref: ref, proxyKey: proxyKey, state: "assigned"}
			h.clientWork[clientID] = ref
			h.proxyWork[proxyKey] = ref
			if status == "queued" {
				task.targets[clientID] = "assigned"
			}
			proxyPosition := task.proxyIndex[proxyID]
			assignment := task.assignment
			assignment.WorkID = workID
			assignment.ProxyIndex = proxyPosition + 1
			assignment.ProxyTotal = len(task.assignment.Proxies)
			assignment.Proxies = []model.ProxySpec{task.assignment.Proxies[proxyPosition]}
			h.scheduleNext = (taskPosition + 1) % len(h.taskOrder)
			return &reservedWork{task: task, connected: connected, assignment: assignment, ref: ref}
		}
	}
	return nil
}

func (h *Hub) nextUnlockedProxy(task *runtimeTask, target *targetWork) (string, [32]byte, bool) {
	for _, proxyID := range target.order {
		if _, done := target.completed[proxyID]; done {
			continue
		}
		key := task.proxyKeys[proxyID]
		if _, locked := h.proxyWork[key]; !locked {
			return proxyID, key, true
		}
	}
	return "", [32]byte{}, false
}

func (h *Hub) reservationActive(reserved *reservedWork) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	task := h.tasks[reserved.ref.taskID]
	if task != reserved.task || task.terminalPending || h.peers[reserved.ref.clientID] != reserved.connected {
		return false
	}
	target := task.work[reserved.ref.clientID]
	return target != nil && target.active != nil && target.active.ref == reserved.ref
}

func (h *Hub) rollbackReservedWork(reserved *reservedWork) {
	h.mu.Lock()
	defer h.mu.Unlock()
	task := h.tasks[reserved.ref.taskID]
	if task != reserved.task {
		return
	}
	target := task.work[reserved.ref.clientID]
	if target == nil || target.active == nil || target.active.ref != reserved.ref {
		return
	}
	h.releaseActiveWorkLocked(target)
	if task.targets[reserved.ref.clientID] == "assigned" {
		task.targets[reserved.ref.clientID] = "queued"
	}
}

func (h *Hub) releaseActiveWorkLocked(target *targetWork) {
	if target == nil || target.active == nil {
		return
	}
	lease := target.active
	if current, ok := h.clientWork[lease.ref.clientID]; ok && current == lease.ref {
		delete(h.clientWork, lease.ref.clientID)
	}
	if current, ok := h.proxyWork[lease.proxyKey]; ok && current == lease.ref {
		delete(h.proxyWork, lease.proxyKey)
	}
	target.active = nil
}

func (h *Hub) releaseTaskWorkLocked(taskID string, task *runtimeTask) {
	for _, target := range task.work {
		h.releaseActiveWorkLocked(target)
	}
	for index, current := range h.taskOrder {
		if current == taskID {
			h.taskOrder = append(h.taskOrder[:index], h.taskOrder[index+1:]...)
			if len(h.taskOrder) == 0 {
				h.scheduleNext = 0
			} else if h.scheduleNext >= len(h.taskOrder) {
				h.scheduleNext = 0
			}
			break
		}
	}
}

func (h *Hub) proxyWorkKey(outbound []byte) [32]byte {
	digest := hmac.New(sha256.New, h.schedulerKey[:])
	_, _ = digest.Write(outbound)
	var key [32]byte
	copy(key[:], digest.Sum(nil))
	return key
}
