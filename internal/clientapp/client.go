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

type Config struct {
	ServerURL string
	Token     string
	Name      string
	Version   string
	Labels    map[string]string
	Logger    *slog.Logger
}

type Client struct {
	config   Config
	executor *Executor
}

type connection struct {
	ws       *websocket.Conn
	writeMu  sync.Mutex
	current  sync.Mutex
	cancel   context.CancelFunc
	taskID   string
	canceled map[string]bool
}

func New(config Config) (*Client, error) {
	if config.ServerURL == "" || config.Token == "" || config.Name == "" {
		return nil, errors.New("server URL, token and client name are required")
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	return &Client{config: config, executor: NewExecutor()}, nil
}

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

func (c *Client) connect(parent context.Context) error {
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
	ws.SetReadLimit(2 << 20)
	connected := &connection{ws: ws, canceled: make(map[string]bool)}
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

	assignments := make(chan model.Assignment, 32)
	errChannel := make(chan error, 2)
	go c.worker(ctx, connected, assignments, errChannel)
	go c.heartbeat(ctx, connected, errChannel)

	for {
		var message wire.Envelope
		if err := wsjson.Read(ctx, ws, &message); err != nil {
			connected.cancelCurrent()
			return err
		}
		if message.Version != model.ProtocolVersion {
			continue
		}
		switch message.Type {
		case wire.TypePong:
		case wire.TypeTaskAssign:
			assignment, err := wire.Decode[model.Assignment](message)
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

func (c *Client) worker(ctx context.Context, connected *connection, assignments <-chan model.Assignment, errorsChannel chan<- error) {
	for {
		select {
		case assignment := <-assignments:
			if connected.consumeCanceled(assignment.TaskID) {
				continue
			}
			timeout := time.Duration(assignment.TimeoutSeconds) * time.Second
			if timeout <= 0 {
				timeout = 10 * time.Minute
			}
			taskCtx, cancel := context.WithTimeout(ctx, timeout)
			connected.setCurrent(assignment.TaskID, cancel)
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

func (c *connection) send(ctx context.Context, message wire.Envelope) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return wsjson.Write(ctx, c.ws, message)
}

func (c *connection) setCurrent(taskID string, cancel context.CancelFunc) {
	c.current.Lock()
	c.taskID, c.cancel = taskID, cancel
	c.current.Unlock()
}

func (c *connection) clearCurrent(taskID string) {
	c.current.Lock()
	if c.taskID == taskID {
		c.taskID, c.cancel = "", nil
	}
	c.current.Unlock()
}

func (c *connection) cancelTask(taskID string) {
	c.current.Lock()
	c.canceled[taskID] = true
	if c.taskID == taskID && c.cancel != nil {
		c.cancel()
	}
	c.current.Unlock()
}

func (c *connection) consumeCanceled(taskID string) bool {
	c.current.Lock()
	defer c.current.Unlock()
	if !c.canceled[taskID] {
		return false
	}
	delete(c.canceled, taskID)
	return true
}

func (c *connection) cancelCurrent() {
	c.current.Lock()
	if c.cancel != nil {
		c.cancel()
	}
	c.current.Unlock()
}

func nonBlockingError(channel chan<- error, err error) {
	select {
	case channel <- err:
	default:
	}
}
