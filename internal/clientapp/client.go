package clientapp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"runtime"
	"strings"
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

// New 校验客户端必填配置并创建执行端。必填值为空、URL 不是 ws/wss、缺少主机或
// 内嵌凭据时会立即失败，以免进入永远无法认证的重连循环。
func New(config Config) (*Client, error) {
	config.ServerURL = strings.TrimSpace(config.ServerURL)
	config.Token = strings.TrimSpace(config.Token)
	config.Name = strings.TrimSpace(config.Name)
	if config.ServerURL == "" || config.Token == "" || config.Name == "" {
		return nil, errors.New("server URL, token and client name are required")
	}
	parsedURL, err := url.Parse(config.ServerURL)
	if err != nil || (parsedURL.Scheme != "ws" && parsedURL.Scheme != "wss") || parsedURL.Host == "" || parsedURL.User != nil {
		return nil, errors.New("server URL must be an absolute ws:// or wss:// URL without credentials")
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
	// Assignment 可能承载接近 5 MiB 的订阅解析结果。服务端在落库前按同一协议常量
	// 校验编码大小，Client 在此设置对应硬上限，既接收合法批量又避免无界内存占用。
	ws.SetReadLimit(wire.MaxServerToClientMessageBytes)
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
