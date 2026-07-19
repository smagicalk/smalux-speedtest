package clientapp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"smalux-speedtest/internal/model"
	"smalux-speedtest/internal/wire"
)

// Config 描述 Client 的连接身份和运行依赖。
type Config struct {
	// ServerURL 是客户端控制面的 ws:// 或 wss:// 地址。
	ServerURL string
	// Token 作为 Bearer Token 放入 WebSocket HTTP Upgrade 请求，用于客户端认证。
	Token string
	// Name 是展示和调度时使用的客户端名称，通常为部署机器的主机名或机房标识。
	Name string
	// Version 是客户端构建版本，会随 Hello 上报给服务端。
	Version string
	// Labels 是随 Hello 上报的自定义元数据，可用于描述地区、运营商或机器规格。
	// Client 只读取该 map；调用方不应在 Run 期间并发修改它。
	Labels map[string]string
	// Logger 接收连接、重连和运行状态日志；为 nil 时使用 slog.Default。
	Logger *slog.Logger
}

// Client 是长连接测速执行端。
//
// Client 保存只读配置和无状态 Executor。一次 Run 调用管理一条逻辑连接的完整生命
// 周期，并在物理 WebSocket 断开后自动重连。Client 没有为并发调用 Run 提供保证。
type Client struct {
	// config 在 New 后保持只读，供重连时重复构造握手信息。
	config Config
	// executor 将 Assignment 转为实际 sing-box 和 speedtest-go 测速流程。
	executor *Executor
}

// connection 封装单次 WebSocket 连接及其任务并发状态。
//
// coder/websocket 允许读写并发，但不允许多个 writer 同时写，因此 writeMu 覆盖所有
// wsjson.Write。current 同时保护当前任务的取消函数和 canceled 集合；后者用于记住
// “取消消息先于 worker 开始任务”这一竞态，防止已进入 assignments 队列的任务仍被执行。
type connection struct {
	// ws 是本次连接的底层 WebSocket，会在 connect 返回时关闭。
	ws *websocket.Conn
	// writeMu 串行化心跳、worker 和取消处理产生的所有消息写入。
	writeMu sync.Mutex

	// current 保护 cancel、taskID 和 canceled 三个任务状态字段。
	current sync.Mutex
	// cancel 终止当前正在执行的 Assignment；空值表示 worker 空闲。
	cancel context.CancelFunc
	// taskID 标识 cancel 当前对应的任务。
	taskID string
	// canceled 记录先于 worker 启动到达的取消消息，消费后删除。
	canceled map[string]bool
}

// New 校验客户端必填配置并创建执行端。ServerURL、Token 和 Name 为空会立即失败，
// 以免进入永远无法认证的重连循环。
func New(config Config) (*Client, error) {
	if config.ServerURL == "" || config.Token == "" || config.Name == "" {
		return nil, errors.New("server URL, token and client name are required")
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	return &Client{config: config, executor: NewExecutor()}, nil
}

// Run 持续维护到服务端的连接，直到 ctx 被取消。
//
// 单次连接失败不会结束客户端，而会以 1 秒起步、上限 30 秒的指数退避重试，并加入
// 0~499ms 随机抖动，避免大量客户端同时重连。ctx 取消会中断连接、等待和当前测速，
// 此时 Run 返回 ctx.Err()。
func (c *Client) Run(ctx context.Context) error {
	delay := time.Second
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := c.connect(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c.config.Logger.Warn("connection closed", "error", err, "retry_in", delay)
		jitter := time.Duration(rand.IntN(500)) * time.Millisecond
		timer := time.NewTimer(delay + jitter)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
		if delay < 30*time.Second {
			delay *= 2
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
		}
	}
}

// connect 建立并服务一次物理 WebSocket 连接；连接关闭后将错误交还 Run 触发重连。
//
// 每次连接包含三个并发角色：当前函数独占读取；worker 串行消费任务；heartbeat 定时
// 发送 Ping。派生 context 在 connect 返回时统一取消，使后两个 goroutine 和当前测速
// 不会跨越连接生命周期继续运行。
func (c *Client) connect(parent context.Context) error {
	// Token 放在 Upgrade 请求头中，避免作为查询参数出现在访问日志或代理日志中。
	header := make(http.Header)
	header.Set("Authorization", "Bearer "+c.config.Token)
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	ws, response, err := websocket.Dial(ctx, c.config.ServerURL, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		if response != nil {
			return fmt.Errorf("websocket dial returned HTTP %d: %w", response.StatusCode, err)
		}
		return err
	}
	defer ws.Close(websocket.StatusNormalClosure, "client shutdown")
	// 限制服务端单条消息大小，避免异常或恶意帧无限占用客户端内存。
	ws.SetReadLimit(2 << 20)
	connected := &connection{ws: ws, canceled: make(map[string]bool)}
	// 应用层握手补充客户端身份信息。HTTP Upgrade 成功不代表协议兼容，必须继续校验
	// Welcome 的消息类型与协议版本。
	hello, _ := wire.New(wire.TypeHello, "", model.Hello{
		Name: c.config.Name, Version: c.config.Version, OS: runtime.GOOS, Arch: runtime.GOARCH, Labels: c.config.Labels,
	})
	if err := connected.send(ctx, hello); err != nil {
		return err
	}
	var welcome wire.Envelope
	welcomeCtx, welcomeCancel := context.WithTimeout(ctx, 10*time.Second)
	err = wsjson.Read(welcomeCtx, ws, &welcome)
	welcomeCancel()
	if err != nil || welcome.Type != wire.TypeWelcome || welcome.Version != model.ProtocolVersion {
		return errors.New("server did not send a valid welcome message")
	}
	identity, _ := wire.Decode[model.Welcome](welcome)
	c.config.Logger.Info("connected to server", "client_id", identity.ClientID, "server", c.config.ServerURL)

	// assignments 提供小型缓冲以解耦读循环和测速 worker；errChannel 只负责通知致命的
	// 写错误。worker 本身仍然一次只执行一个 Assignment。
	assignments := make(chan model.Assignment, 32)
	errChannel := make(chan error, 2)
	go c.worker(ctx, connected, assignments, errChannel)
	go c.heartbeat(ctx, connected, errChannel)

	for {
		var message wire.Envelope
		// 当前 goroutine 是该连接唯一 reader；读失败通常意味着连接已经关闭。
		if err := wsjson.Read(ctx, ws, &message); err != nil {
			connected.cancelCurrent()
			return err
		}
		// 忽略不兼容版本，避免用当前数据结构误解未来协议载荷。
		if message.Version != model.ProtocolVersion {
			continue
		}
		switch message.Type {
		case wire.TypePong:
		case wire.TypeTaskAssign:
			assignment, err := wire.Decode[model.Assignment](message)
			// Envelope 与 Payload 中的 TaskID 必须一致，防止错误关联任务状态。
			if err == nil && assignment.TaskID == message.TaskID {
				select {
				case assignments <- assignment:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		case wire.TypeTaskCancel:
			connected.cancelTask(message.TaskID)
		}
		select {
		case err := <-errChannel:
			return err
		default:
		}
	}
}

// worker 串行执行任务并把状态机事件写回服务端。
//
// 每个任务依次经历 ACK、若干 Progress/Result、Complete 或 Failed。Assignment 的超时
// 会派生为 taskCtx；服务端取消任务时，connection.cancelTask 取消同一个 context。
// Progress 属于尽力而为的瞬时信息，发送失败不会丢弃最终测速结果；结果和终态消息发送
// 失败则强制关闭连接，由 Run 重新建立控制通道。
func (c *Client) worker(ctx context.Context, connected *connection, assignments <-chan model.Assignment, errorsChannel chan<- error) {
	for {
		select {
		case assignment := <-assignments:
			// cancel 可能在 Assignment 排队期间到达；开始任务前消费该标记即可避免误执行。
			if connected.consumeCanceled(assignment.TaskID) {
				continue
			}
			timeout := time.Duration(assignment.TimeoutSeconds) * time.Second
			if timeout <= 0 {
				timeout = 10 * time.Minute
			}
			taskCtx, cancel := context.WithTimeout(ctx, timeout)
			connected.setCurrent(assignment.TaskID, cancel)
			// ACK 表示客户端已开始接管任务，而不是测速已经成功。
			ack, _ := wire.New(wire.TypeTaskAck, assignment.TaskID, model.Ack{TaskID: assignment.TaskID})
			if err := connected.send(ctx, ack); err != nil {
				cancel()
				connected.clearCurrent(assignment.TaskID)
				nonBlockingError(errorsChannel, err)
				connected.ws.CloseNow()
				return
			}
			results := c.executor.Execute(taskCtx, assignment, func(progress model.Progress) {
				message, _ := wire.New(wire.TypeTaskProgress, assignment.TaskID, progress)
				writeCtx, writeCancel := context.WithTimeout(ctx, 5*time.Second)
				// 进度只用于实时展示；短暂拥塞时允许丢失，最终 Result 才是持久化依据。
				_ = connected.send(writeCtx, message)
				writeCancel()
			})
			for _, result := range results {
				message, _ := wire.New(wire.TypeTaskResult, assignment.TaskID, result)
				writeCtx, writeCancel := context.WithTimeout(ctx, 10*time.Second)
				err := connected.send(writeCtx, message)
				writeCancel()
				if err != nil {
					cancel()
					connected.clearCurrent(assignment.TaskID)
					nonBlockingError(errorsChannel, err)
					connected.ws.CloseNow()
					return
				}
			}
			var terminal wire.Envelope
			// Execute 可能在取消前已经产生部分 Result；这些结果仍会先发送，随后以 Failed
			// 明确标记任务未完整结束。
			if err := taskCtx.Err(); err != nil {
				terminal, _ = wire.New(wire.TypeTaskFailed, assignment.TaskID, model.Failure{TaskID: assignment.TaskID, Error: err.Error()})
			} else {
				terminal, _ = wire.New(wire.TypeTaskComplete, assignment.TaskID, model.Ack{TaskID: assignment.TaskID})
			}
			writeCtx, writeCancel := context.WithTimeout(ctx, 10*time.Second)
			err := connected.send(writeCtx, terminal)
			writeCancel()
			cancel()
			connected.clearCurrent(assignment.TaskID)
			if err != nil {
				nonBlockingError(errorsChannel, err)
				connected.ws.CloseNow()
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// heartbeat 每 20 秒发送一次应用层 Ping。写入最多等待 5 秒；超时或连接错误会关闭
// WebSocket，以便阻塞中的读取立即返回并进入重连流程。
func (c *Client) heartbeat(ctx context.Context, connected *connection, errorsChannel chan<- error) {
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			message, _ := wire.New(wire.TypePing, "", struct{}{})
			writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := connected.send(writeCtx, message)
			cancel()
			if err != nil {
				nonBlockingError(errorsChannel, err)
				connected.ws.CloseNow()
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// send 是 connection 唯一的 WebSocket 写入口。writeMu 保证心跳、任务进度和任务结果
// 不会并发调用 wsjson.Write；ctx 则为每类消息提供各自的写入截止时间。
func (c *connection) send(ctx context.Context, message wire.Envelope) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return wsjson.Write(ctx, c.ws, message)
}

// setCurrent 发布当前任务及其取消函数，使读取循环收到 task.cancel 时能够中断 worker。
func (c *connection) setCurrent(taskID string, cancel context.CancelFunc) {
	c.current.Lock()
	c.taskID, c.cancel = taskID, cancel
	c.current.Unlock()
}

// clearCurrent 仅在 taskID 仍匹配时清理状态，避免迟到的旧任务收尾覆盖新任务状态。
func (c *connection) clearCurrent(taskID string) {
	c.current.Lock()
	if c.taskID == taskID {
		c.taskID, c.cancel = "", nil
	}
	c.current.Unlock()
}

// cancelTask 记录取消意图并取消同 ID 的运行中任务。即使任务还在队列中，canceled
// 标记也会由 worker 在执行前识别。
func (c *connection) cancelTask(taskID string) {
	c.current.Lock()
	c.canceled[taskID] = true
	if c.taskID == taskID && c.cancel != nil {
		c.cancel()
	}
	c.current.Unlock()
}

// consumeCanceled 原子地查询并消费排队任务的取消标记。
func (c *connection) consumeCanceled(taskID string) bool {
	c.current.Lock()
	defer c.current.Unlock()
	if !c.canceled[taskID] {
		return false
	}
	delete(c.canceled, taskID)
	return true
}

// cancelCurrent 在连接读失败时中断当前测速，防止失去控制通道后仍持续消耗带宽。
func (c *connection) cancelCurrent() {
	c.current.Lock()
	if c.cancel != nil {
		c.cancel()
	}
	c.current.Unlock()
}

// nonBlockingError 尝试上报 goroutine 的致命错误。错误通道已满时直接丢弃，因为首个
// 错误已经足以触发连接关闭，生产者不能因重复报错而阻塞退出。
func nonBlockingError(channel chan<- error, err error) {
	select {
	case channel <- err:
	default:
	}
}
