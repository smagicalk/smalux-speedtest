package serverapp

import (
	"context"
	"sync"
	"time"
)

// hubTransitionContext 让关键状态写入不随 WebSocket 请求取消而中断，同时保留调用方
// 的值和总 Deadline，并设置五秒硬上限，防止 SQLite 故障永久占用任务转换锁。
func hubTransitionContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	// WithoutCancel 避免浏览器断开使已开始的状态事务半途取消，但它也会移除 Deadline。
	// 显式取两者较早值，确保 App.Shutdown 的总期限不会被每个任务重新延长五秒。
	deadline := time.Now().Add(5 * time.Second)
	if parentDeadline, ok := ctx.Deadline(); ok && parentDeadline.Before(deadline) {
		deadline = parentDeadline
	}
	return context.WithDeadline(context.WithoutCancel(ctx), deadline)
}

// lockTaskTransition 等待任务转换锁时响应调用方取消，并以五秒为无 Deadline 请求
// 设置硬上限。锁尚未取得时没有正在进行的状态事务，因此这里保留原始 Context 的
// cancellation；取得锁后的 SQLite 写入才使用 hubTransitionContext 脱离请求取消。
// sync.Mutex 没有 Context API，因此使用低频 TryLock 轮询。它只用于可能从 Shutdown
// 进入的取消路径，正常 ACK、结果和断线转换仍直接使用互斥锁。
func lockTaskTransition(ctx context.Context, mutex *sync.Mutex) error {
	if ctx == nil {
		ctx = context.Background()
	}
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(2 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-waitCtx.Done():
			return waitCtx.Err()
		default:
		}
		if mutex.TryLock() {
			return nil
		}
		select {
		case <-waitCtx.Done():
			return waitCtx.Err()
		case <-ticker.C:
		}
	}
}
