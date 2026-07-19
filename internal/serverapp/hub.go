package serverapp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"smalux-speedtest/internal/model"
	"smalux-speedtest/internal/store"
	"smalux-speedtest/internal/wire"
)

// peer 表示一个已完成认证和 hello 握手的 Client WebSocket 连接。
type peer struct {
	// client 是连接建立时加载并由 hello 信息更新后的 Client 快照。
	client store.Client
	// conn 由 ServeWebSocket 的读循环读取，也可能被调度、取消、心跳等路径写入。
	conn *websocket.Conn
	// write 保证每个连接同时最多只有一个 WebSocket Writer。coder/websocket 支持并发
	// 读写，但不允许多个业务 goroutine 无序并发写帧。
	write sync.Mutex
}

// send 在 peer 级写锁保护下发送一个协议信封；ctx 决定单次写入的超时或取消边界。
func (p *peer) send(ctx context.Context, envelope wire.Envelope) error {
	p.write.Lock()
	defer p.write.Unlock()
	return wsjson.Write(ctx, p.conn, envelope)
}

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

// taskEvent 是 Hub 推送给管理页面 SSE 的内部事件表示。
// 每个事件按 Type 只使用对应载荷字段，omitempty 可减少流式消息大小。
type taskEvent struct {
	// Type 决定前端应读取 progress、result 还是 status 字段。
	Type string `json:"type"`
	// Progress 是 task.progress 事件的瞬时进度载荷。
	Progress *model.Progress `json:"progress,omitempty"`
	// Result 是成功落库后广播的单条测速结果。
	Result *model.SpeedResult `json:"result,omitempty"`
	// Status 是任务或目标聚合后的状态名称。
	Status string `json:"status,omitempty"`
	// Message 提供状态事件的可选补充说明。
	Message string `json:"message,omitempty"`
}

// Hub 协调 Client 长连接、运行中任务状态机和浏览器事件订阅。
//
// peers、tasks、subscribers 统一由 mu 保护。方法只应在锁内读取或修改这些 map 和
// runtimeTask.targets；WebSocket I/O、SQLite I/O 及事件投递尽量在释放 Hub 锁后进行，
// 避免慢 Client 或磁盘操作阻塞其他连接。
type Hub struct {
	// store 是 Client 认证、目标状态、任务状态和结果的持久化边界。
	store *store.Store
	// log 记录连接和调度诊断信息，不记录代理配置或 Client Token。
	log *slog.Logger

	// mu 保护 peers、tasks、subscribers 及 runtimeTask.targets。
	mu sync.RWMutex
	// peers 以 Client ID 为键，每个 Client 同时只保留最新一条连接。
	peers map[string]*peer
	// tasks 只包含当前进程内仍活跃、且需要保留敏感 Assignment 的任务。
	tasks map[string]*runtimeTask
	// subscribers 按 Task ID 保存 SSE 事件 channel 集合。
	subscribers map[string]map[chan taskEvent]struct{}
}

// NewHub 创建一个没有在线 Client、活动任务和 SSE 订阅者的 Hub。
func NewHub(store *store.Store, logger *slog.Logger) *Hub {
	return &Hub{
		store:       store,
		log:         logger,
		peers:       make(map[string]*peer),
		tasks:       make(map[string]*runtimeTask),
		subscribers: make(map[string]map[chan taskEvent]struct{}),
	}
}

// Online 报告指定 Client 是否在当前 Hub 中登记了完成握手的连接。
// 这是瞬时运行状态，并不等价于数据库中的 Enabled 或 LastSeen。
func (h *Hub) Online(clientID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.peers[clientID]
	return ok
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

// ServeWebSocket 完成 Client 认证、协议握手、连接登记和消息读取循环。
//
// 认证分两层：HTTP Upgrade 前用数据库中的 Client Bearer Token 验证身份；Upgrade 后首帧
// 必须是版本匹配且名称非空的 client.hello。管理员 Session Cookie 不参与此端点认证。
func (h *Hub) ServeWebSocket(w http.ResponseWriter, r *http.Request) {
	// 在升级协议前拒绝无效或已撤销 Token，避免为未认证请求分配长连接资源。
	token := bearerToken(r.Header.Get("Authorization"))
	client, err := h.store.AuthenticateClient(r.Context(), token)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// Client 是非浏览器 Agent，Origin 不是认证凭据，因此允许任意 Origin；安全性来自
	// Authorization Bearer Token。若未来允许浏览器 Client，应重新收紧此策略。
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
	if err != nil {
		return
	}
	// 限制单条消息读取大小，防止异常 Client 通过超大 JSON 占用服务端内存。
	conn.SetReadLimit(2 << 20)
	defer conn.Close(websocket.StatusNormalClosure, "connection closed")

	// 要求 Client 在 10 秒内发送首个 hello，防止只完成 Upgrade 却不握手的空闲连接。
	helloCtx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	var first wire.Envelope
	err = wsjson.Read(helloCtx, conn, &first)
	cancel()
	if err != nil || first.Version != model.ProtocolVersion || first.Type != wire.TypeHello {
		conn.Close(websocket.StatusPolicyViolation, "client.hello required")
		return
	}
	hello, err := wire.Decode[model.Hello](first)
	if err != nil || strings.TrimSpace(hello.Name) == "" {
		conn.Close(websocket.StatusPolicyViolation, "invalid client.hello")
		return
	}
	// hello 中的版本、平台和标签属于可持久化运行元数据，不包含认证 Token。
	if err := h.store.UpdateClientHello(r.Context(), client.ID, hello); err != nil {
		conn.Close(websocket.StatusInternalError, "failed to register client")
		return
	}
	client.Name, client.Version, client.OS, client.Arch, client.Labels = hello.Name, hello.Version, hello.OS, hello.Arch, hello.Labels
	connected := &peer{client: client, conn: conn}
	// register 可能替换同一 Client 的旧连接；defer unregister 通过指针身份检查，确保
	// 旧连接退出时不会误删刚登记的新连接。
	h.register(connected)
	defer h.unregister(connected)

	welcome, _ := wire.New(wire.TypeWelcome, "", model.Welcome{ClientID: client.ID})
	writeCtx, writeCancel := context.WithTimeout(r.Context(), 5*time.Second)
	if err := connected.send(writeCtx, welcome); err != nil {
		writeCancel()
		return
	}
	writeCancel()
	// welcome 写成功后才补发积压任务，保证 Client 已得知服务端确认的 Client ID。
	h.dispatchQueued(client.ID)
	h.log.Info("client connected", "client_id", client.ID, "name", hello.Name, "remote", r.RemoteAddr)

	// 每条连接只有此 goroutine 读取；消息写入则统一通过 peer.send 串行化。
	for {
		var message wire.Envelope
		if err := wsjson.Read(r.Context(), conn, &message); err != nil {
			return
		}
		// 忽略不兼容版本的后续消息，避免按错误载荷语义更新任务状态。
		if message.Version != model.ProtocolVersion {
			continue
		}
		h.handleMessage(r.Context(), connected, message)
	}
}

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

// handleMessage 按 wire.Type 驱动任务状态机。
//
// 心跳只刷新 Client 活跃时间；ACK 将 queued 转为 running；进度只广播不持久化；结果在
// 确认目标仍为 running 后持久化；complete/failed 则完成单个目标并触发任务聚合。无法
// 解码或 TaskID 自相矛盾的消息会被忽略，不允许污染其他任务。
func (h *Hub) handleMessage(ctx context.Context, connected *peer, message wire.Envelope) {
	switch message.Type {
	case wire.TypePing:
		// Pong 与 TouchClient 都是尽力而为：短暂写入或数据库错误不应直接中断读循环。
		response, _ := wire.New(wire.TypePong, "", struct{}{})
		writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_ = connected.send(writeCtx, response)
		cancel()
		_ = h.store.TouchClient(ctx, connected.client.ID)
	case wire.TypeTaskAck:
		h.setTargetRunning(ctx, message.TaskID, connected.client.ID)
	case wire.TypeTaskProgress:
		// 进度频率较高且只用于实时展示，因此不写入 SQLite。
		progress, err := wire.Decode[model.Progress](message)
		if err == nil && progress.TaskID == message.TaskID {
			h.publish(message.TaskID, taskEvent{Type: "progress", Progress: &progress})
		}
	case wire.TypeTaskResult:
		result, err := wire.Decode[model.SpeedResult](message)
		if err == nil && result.TaskID == message.TaskID && h.acceptsResult(message.TaskID, connected.client.ID) {
			// ClientID 取认证连接身份而非信任 Client 载荷，防止节点冒充其他 Client。
			result.ClientID = connected.client.ID
			// 先落库再广播，保证收到 SSE result 后通过 REST 刷新可读取到相同结果。
			if err := h.store.SaveResult(ctx, result); err == nil {
				h.publish(message.TaskID, taskEvent{Type: "result", Result: &result})
			}
		}
	case wire.TypeTaskComplete:
		h.finishTarget(ctx, message.TaskID, connected.client.ID, "completed", "")
	case wire.TypeTaskFailed:
		failure, err := wire.Decode[model.Failure](message)
		if err == nil {
			h.finishTarget(ctx, message.TaskID, connected.client.ID, "failed", failure.Error)
		}
	}
}

// setTargetRunning 处理 Client 对任务分配的 ACK。
// 只有 queued -> running 是有效转换，重复 ACK、未知任务和非目标 Client 都是幂等空操作；
// 内存转换在锁内完成，持久化和 SSE 发布在锁外完成。
func (h *Hub) setTargetRunning(ctx context.Context, taskID, clientID string) {
	h.mu.Lock()
	task := h.tasks[taskID]
	transitioned := false
	if task != nil && task.targets[clientID] == "queued" {
		task.targets[clientID] = "running"
		transitioned = true
	}
	h.mu.Unlock()
	if !transitioned {
		return
	}
	_ = h.store.SetTargetStatus(ctx, taskID, clientID, "running", "")
	_ = h.store.SetTaskStatus(ctx, taskID, "running", "")
	h.publish(taskID, taskEvent{Type: "status", Status: "running"})
}

// acceptsResult 仅允许活动任务中处于 running 的目标提交结果。
// 这会拒绝未 ACK、已取消、已完成或超时后的迟到结果。
func (h *Hub) acceptsResult(taskID, clientID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	task := h.tasks[taskID]
	return task != nil && task.targets[clientID] == "running"
}

// finishTarget 将单个 Client 目标推进到给定终态，然后根据所有目标状态聚合任务。
// 对已在 completed/failed/canceled 的目标保持幂等，避免断线、超时和重复消息并发时重复
// 收敛。调用者当前只传入受控的终态字符串。
func (h *Hub) finishTarget(ctx context.Context, taskID, clientID, status, detail string) {
	h.mu.Lock()
	task := h.tasks[taskID]
	if task == nil {
		h.mu.Unlock()
		return
	}
	current := task.targets[clientID]
	if current == "completed" || current == "failed" || current == "canceled" {
		h.mu.Unlock()
		return
	}
	task.targets[clientID] = status
	h.mu.Unlock()
	_ = h.store.SetTargetStatus(ctx, taskID, clientID, status, detail)
	h.aggregate(ctx, taskID)
}

// aggregate 从数据库读取目标状态汇总；只有所有目标均为终态时才确定任务最终状态：
//   - 全部完成，任务为 completed；
//   - 全部取消，任务为 canceled；
//   - 全部失败，任务为 failed；
//   - 完成、失败或取消混合，任务为 partial。
//
// 数据库成为聚合依据，避免仅依赖进程内 map 与已持久化状态发生偏差。确定终态后停止
// 超时器、清空代理配置、删除 runtimeTask，最后向 SSE 订阅者发布状态。
func (h *Hub) aggregate(ctx context.Context, taskID string) {
	completed, failed, canceled, total, err := h.store.TargetSummary(ctx, taskID)
	if err != nil || completed+failed+canceled < total {
		return
	}
	status := "completed"
	detail := ""
	if canceled == total {
		status = "canceled"
	} else if failed == total {
		status = "failed"
		detail = "all clients failed"
	} else if failed > 0 || canceled > 0 {
		status = "partial"
		detail = "some clients did not complete"
	}
	_ = h.store.SetTaskStatus(ctx, taskID, status, detail)
	h.mu.Lock()
	if task := h.tasks[taskID]; task != nil {
		if task.expires != nil {
			task.expires.Stop()
		}
		// 主动断开对敏感代理字段的引用，使其尽早具备垃圾回收条件。
		task.assignment.Proxies = nil
		delete(h.tasks, taskID)
	}
	h.mu.Unlock()
	h.publish(taskID, taskEvent{Type: "status", Status: status, Message: detail})
}

// expireTask 是 AddTask 定时器的回调。
// 它先在读锁内快照 queued/running 目标，再逐个走 finishTarget 的正常失败路径；这样
// 数据库、聚合和 SSE 行为与 Client 主动失败保持一致，也能容忍回调与完成消息并发。
func (h *Hub) expireTask(taskID string) {
	h.mu.RLock()
	task := h.tasks[taskID]
	if task == nil {
		h.mu.RUnlock()
		return
	}
	var pending []string
	for clientID, status := range task.targets {
		if status == "queued" || status == "running" {
			pending = append(pending, clientID)
		}
	}
	h.mu.RUnlock()
	for _, clientID := range pending {
		h.finishTarget(context.Background(), taskID, clientID, "failed", "task expired after 10 minutes")
	}
}

// Subscribe 为指定任务注册一个容量为 32 的 SSE 事件队列，并返回幂等风格的注销函数。
// channel 不会在注销时 close，因为 publish 可能已经取得其快照；不关闭可避免并发发送
// 导致 panic。HTTP handler 通过请求 Context 退出，不依赖 channel 关闭信号。
func (h *Hub) Subscribe(taskID string) (<-chan taskEvent, func()) {
	channel := make(chan taskEvent, 32)
	h.mu.Lock()
	if h.subscribers[taskID] == nil {
		h.subscribers[taskID] = make(map[chan taskEvent]struct{})
	}
	h.subscribers[taskID][channel] = struct{}{}
	h.mu.Unlock()
	return channel, func() {
		h.mu.Lock()
		delete(h.subscribers[taskID], channel)
		if len(h.subscribers[taskID]) == 0 {
			delete(h.subscribers, taskID)
		}
		h.mu.Unlock()
	}
}

// publish 将事件尽力广播给任务的当前订阅者。
//
// 先在读锁内复制 channel 列表，再在锁外执行非阻塞发送。慢浏览器队列满时会丢弃增量
// 事件而不会反压 WebSocket/SQLite 主流程；前端可通过任务 REST API 重新获取权威快照。
func (h *Hub) publish(taskID string, event taskEvent) {
	h.mu.RLock()
	channels := make([]chan taskEvent, 0, len(h.subscribers[taskID]))
	for channel := range h.subscribers[taskID] {
		channels = append(channels, channel)
	}
	h.mu.RUnlock()
	for _, channel := range channels {
		select {
		case channel <- event:
		default:
		}
	}
}

// bearerToken 从 Authorization Header 解析大小写不敏感的 Bearer 方案。
// 格式不正确时返回空串，随后统一由 store.AuthenticateClient 判定为未认证。
func bearerToken(header string) string {
	parts := strings.SplitN(header, " ", 2)
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		return strings.TrimSpace(parts[1])
	}
	return ""
}

// eventJSON 把内部事件编码为 SSE data 行的 JSON；taskEvent 只包含可编码字段，因此沿用
// 无错误返回的简化接口。
func eventJSON(event taskEvent) []byte {
	value, _ := json.Marshal(event)
	return value
}
