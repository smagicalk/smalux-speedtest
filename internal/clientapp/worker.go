package clientapp

import (
	"context"
	"time"

	"smalux-speedtest/internal/model"
	"smalux-speedtest/internal/wire"
)

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

// nonBlockingError 尝试上报 goroutine 的致命错误。错误通道已满时直接丢弃，因为首个
// 错误已经足以触发连接关闭，生产者不能因重复报错而阻塞退出。
func nonBlockingError(channel chan<- error, err error) {
	select {
	case channel <- err:
	default:
	}
}
