package serverapp

import (
	"context"
	"errors"
	"net/http"
	"time"

	"smalux-speedtest/internal/model"
	"smalux-speedtest/internal/reportpng"
	"smalux-speedtest/internal/store"
	"smalux-speedtest/internal/telegrambot"
)

// telegramTaskRunner 将 Bot 的简化请求接入与管理页面共用的任务服务。
// Bot 不接受任意 Client ID，而是选择提交时全部“已启用且在线”的 Client，避免任务因
// 长期离线目标一直停留在 queued。
type telegramTaskRunner struct {
	app *App
}

// telegramTaskPayload 是任务进入终态后一次性读取的报告快照。
// Renderer 只接触脱敏后的 SpeedResult，不会收到原始代理分享链接或 outbound。
type telegramTaskPayload struct {
	Task         store.Task
	Results      []model.SpeedResult
	TotalResults int
}

func (r telegramTaskRunner) Submit(ctx context.Context, request telegrambot.TaskRequest) (telegrambot.Task, error) {
	clientIDs, err := r.onlineClientIDs(ctx)
	if err != nil {
		return telegrambot.Task{}, err
	}
	if len(clientIDs) == 0 {
		return telegrambot.Task{}, telegrambot.NewUserError("当前没有在线且已启用的 Client。")
	}
	created, err := r.app.startTask(ctx, taskRequest{
		Source: request.Source, SubscriptionURL: request.SubscriptionURL, ClientIDs: clientIDs,
	})
	if err != nil {
		var requestError *taskRequestError
		if errors.As(err, &requestError) {
			// 网络库错误可能包含带鉴权查询参数的完整订阅 URL，Bot 消息和普通日志都不
			// 应复述它。代理解析及其他输入错误本身不包含原始输入，可以安全展示。
			if request.SubscriptionURL != "" && requestError.Status != http.StatusRequestEntityTooLarge && requestError.Message != "没有可测速的代理" {
				return telegrambot.Task{}, telegrambot.NewUserError("订阅获取失败，请检查地址后重试。")
			}
			return telegrambot.Task{}, telegrambot.NewUserError(requestError.Message)
		}
		return telegrambot.Task{}, err
	}
	return telegrambot.Task{ID: created.Task.ID}, nil
}

// Wait 先订阅 Hub 事件再读取数据库快照，避免任务恰好在两步之间结束造成漏通知。
// 两秒一次的数据库复核同时覆盖事件队列拥塞和非 Hub 状态更新，是最终权威兜底。
func (r telegramTaskRunner) Wait(ctx context.Context, taskID string) (telegrambot.Completion, error) {
	events, unsubscribe := r.app.hub.Subscribe(taskID)
	defer unsubscribe()
	completion, done, err := r.readCompletion(ctx, taskID)
	if err != nil || done {
		return completion, err
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case event := <-events:
			if event.Type == "status" && isTerminalTaskStatus(event.Status) {
				completion, done, err := r.readCompletion(ctx, taskID)
				if err != nil || done {
					return completion, err
				}
			}
		case <-ticker.C:
			completion, done, err := r.readCompletion(ctx, taskID)
			if err != nil || done {
				return completion, err
			}
		case <-ctx.Done():
			return telegrambot.Completion{}, ctx.Err()
		}
	}
}

func (r telegramTaskRunner) Cancel(ctx context.Context, taskID string) error {
	return r.app.hub.CancelTask(ctx, taskID)
}

func (r telegramTaskRunner) onlineClientIDs(ctx context.Context) ([]string, error) {
	clients, err := r.app.store.ListClients(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(clients))
	for _, client := range clients {
		if client.Enabled && r.app.hub.Online(client.ID) {
			ids = append(ids, client.ID)
		}
	}
	return ids, nil
}

func (r telegramTaskRunner) readCompletion(ctx context.Context, taskID string) (telegrambot.Completion, bool, error) {
	task, err := r.app.store.GetTask(ctx, taskID)
	if err != nil {
		return telegrambot.Completion{}, false, err
	}
	if !isTerminalTaskStatus(task.Status) {
		return telegrambot.Completion{}, false, nil
	}
	completion, err := r.completedSnapshotForTask(ctx, task)
	return completion, true, err
}

func (r telegramTaskRunner) completedSnapshot(ctx context.Context, taskID string) (telegrambot.Completion, error) {
	task, err := r.app.store.GetTask(ctx, taskID)
	if err != nil {
		return telegrambot.Completion{}, err
	}
	return r.completedSnapshotForTask(ctx, task)
}

func (r telegramTaskRunner) completedSnapshotForTask(ctx context.Context, task store.Task) (telegrambot.Completion, error) {
	totalResults, err := r.app.store.CountResults(ctx, task.ID)
	if err != nil {
		return telegrambot.Completion{}, err
	}
	results, err := r.app.store.ListResultsLimited(ctx, task.ID, reportpng.MaxRows)
	if err != nil {
		return telegrambot.Completion{}, err
	}
	return telegrambot.Completion{
		TaskID: task.ID, Status: task.Status, Message: task.Error,
		Payload: telegramTaskPayload{Task: task, Results: results, TotalResults: totalResults},
	}, nil
}

func isTerminalTaskStatus(status string) bool {
	switch status {
	case "completed", "partial", "failed", "canceled":
		return true
	default:
		return false
	}
}

var _ telegrambot.Runner = telegramTaskRunner{}
var _ telegrambot.Canceler = telegramTaskRunner{}
