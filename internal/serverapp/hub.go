package serverapp

import (
	"context"
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
