package serverapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"smalux-speedtest/internal/model"
	"smalux-speedtest/internal/reportpng"
	"smalux-speedtest/internal/store"
	"smalux-speedtest/internal/telegrambot"
)

// telegramTaskRunner 将 Bot 的简化请求接入与管理页面共用的任务服务。
// Bot 只接受提交时“已启用且在线”的 Client ID；快速提交未显式选择时使用全部可用
// Client，避免任务因长期离线目标一直停留在 queued。
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
	available, err := r.ListAvailableClients(ctx)
	if err != nil {
		return telegrambot.Task{}, err
	}
	if len(available) == 0 {
		return telegrambot.Task{}, telegrambot.NewUserError("当前没有在线且已启用的 Client。")
	}
	selected, err := selectTelegramClients(available, request.ClientIDs)
	if err != nil {
		return telegrambot.Task{}, err
	}
	clientIDs := make([]string, 0, len(selected))
	for _, client := range selected {
		clientIDs = append(clientIDs, client.ID)
	}
	created, err := r.app.startTask(ctx, taskRequest{
		Source: request.Source, SubscriptionURL: request.SubscriptionURL, ClientIDs: clientIDs,
		CandidateCount: request.CandidateCount, TopN: request.TopN, Threads: request.Threads,
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
	return telegrambot.Task{ID: created.Task.ID, ClientCount: created.Task.ClientCount, ProxyCount: created.Task.ProxyCount, TopN: created.Task.TopN, Clients: selected}, nil
}

// Wait 先订阅 Hub 事件再读取数据库快照，避免任务恰好在两步之间结束造成漏通知。
// 两秒一次的数据库复核同时覆盖事件队列拥塞和非 Hub 状态更新，是最终权威兜底。
func (r telegramTaskRunner) Wait(ctx context.Context, taskID string) (telegrambot.Completion, error) {
	return r.wait(ctx, taskID, nil)
}

// WaitWithProgress 把 Hub 已脱敏的阶段、速率和结果计数转为 Bot 进度；回调不接触
// Assignment 或原始代理内容。Wait 保留给不需要进度的调用方。
func (r telegramTaskRunner) WaitWithProgress(ctx context.Context, taskID string, onProgress func(telegrambot.TaskProgress)) (telegrambot.Completion, error) {
	return r.wait(ctx, taskID, onProgress)
}

func (r telegramTaskRunner) wait(ctx context.Context, taskID string, onProgress func(telegrambot.TaskProgress)) (telegrambot.Completion, error) {
	events, unsubscribe := r.app.hub.Subscribe(taskID)
	defer unsubscribe()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	results := 0
	clientNames := make(map[string]string)
	if clients, err := r.app.store.ListClients(ctx); err == nil {
		for _, client := range clients {
			clientNames[client.ID] = client.Name
		}
	}
	if persisted, countErr := r.app.store.CountResults(ctx, taskID); countErr == nil && persisted > 0 {
		results = persisted
		if onProgress != nil {
			onProgress(telegrambot.TaskProgress{Status: "running", Results: results})
		}
	}
	for {
		completion, done, err := r.readCompletion(ctx, taskID)
		if err != nil {
			if ctx.Err() != nil {
				return telegrambot.Completion{}, ctx.Err()
			}
			// SQLite 或短暂连接故障不代表测速任务失败。继续复核，避免 Bot 提前
			// 清除 active 状态并给用户一个永远不会到达的失败图片。
			r.app.config.Logger.Warn("telegram task status read failed; retrying", "task_id", taskID, "error_type", fmt.Sprintf("%T", err))
			if !waitTelegramRetry(ctx, 2*time.Second) {
				return telegrambot.Completion{}, ctx.Err()
			}
			continue
		}
		if done {
			return completion, nil
		}
		select {
		case event := <-events:
			if onProgress != nil {
				switch event.Type {
				case "progress":
					if event.Progress != nil {
						onProgress(telegrambot.TaskProgress{Status: "running", Phase: event.Progress.Phase, ProxyName: event.Progress.ProxyName, Message: event.Progress.Message, Current: event.Progress.Current, Total: event.Progress.Total, RateBPS: event.Progress.RateBPS, Results: results, ClientID: event.Progress.ClientID, ClientName: event.Progress.ClientName})
					}
				case "result":
					results++
					progress := telegrambot.TaskProgress{Status: "running", Phase: "汇总结果", Message: "已收到 Client 测速结果", Results: results}
					if event.Result != nil {
						progress.ClientID = event.Result.ClientID
						progress.ClientName = clientNames[event.Result.ClientID]
					}
					onProgress(progress)
				case "target":
					onProgress(telegrambot.TaskProgress{Status: "running", Results: results, ClientID: event.ClientID, ClientName: clientNames[event.ClientID], TargetStatus: event.TargetStatus})
				case "status":
					onProgress(telegrambot.TaskProgress{Status: event.Status, Phase: telegramTaskStatusText(event.Status), Message: event.Message, Results: results})
				}
			}
			if event.Type == "status" && isTerminalTaskStatus(event.Status) {
				// 下一轮立即读取权威快照；若此刻 SQLite 暂时不可用，会走上面的
				// 有界退避，而不是把事件误判为任务失败。
				continue
			}
		case <-ticker.C:
			// 定时复核覆盖事件丢失、服务重启和非 Hub 状态更新。结果事件可能在
			// Submit 返回、Wait 完成订阅之前到达，因此同时用持久化计数校准总体进度。
			if persisted, countErr := r.app.store.CountResults(ctx, taskID); countErr == nil && persisted > results {
				results = persisted
				if onProgress != nil {
					onProgress(telegrambot.TaskProgress{Status: "running", Results: results})
				}
			}
		case <-ctx.Done():
			return telegrambot.Completion{}, ctx.Err()
		}
	}
}

func telegramTaskStatusText(status string) string {
	switch status {
	case "queued":
		return "等待 Client"
	case "running":
		return "任务执行中"
	case "completed":
		return "测速完成"
	case "partial":
		return "部分完成"
	case "failed":
		return "测速失败"
	case "canceled":
		return "已取消"
	default:
		return status
	}
}

// waitTelegramRetry 在数据库短暂不可用时等待下一次状态复核，同时尊重 Bot 关闭信号。
func waitTelegramRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func (r telegramTaskRunner) Cancel(ctx context.Context, taskID string) error {
	return r.app.hub.CancelTask(ctx, taskID)
}

func (r telegramTaskRunner) onlineClientIDs(ctx context.Context) ([]string, error) {
	clients, err := r.ListAvailableClients(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(clients))
	for _, client := range clients {
		ids = append(ids, client.ID)
	}
	return ids, nil
}

func (r telegramTaskRunner) ListAvailableClients(ctx context.Context) ([]telegrambot.ClientOption, error) {
	clients, err := r.app.store.ListClients(ctx)
	if err != nil {
		return nil, err
	}
	options := make([]telegrambot.ClientOption, 0, len(clients))
	for _, client := range clients {
		if client.Enabled && r.app.hub.Online(client.ID) {
			options = append(options, telegrambot.ClientOption{ID: client.ID, Name: client.Name})
		}
	}
	return options, nil
}

func selectTelegramClients(available []telegrambot.ClientOption, requested []string) ([]telegrambot.ClientOption, error) {
	if len(requested) == 0 {
		return append([]telegrambot.ClientOption(nil), available...), nil
	}
	byID := make(map[string]telegrambot.ClientOption, len(available))
	for _, client := range available {
		byID[client.ID] = client
	}
	selected := make([]telegrambot.ClientOption, 0, len(requested))
	seen := make(map[string]struct{}, len(requested))
	for _, id := range requested {
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		client, ok := byID[id]
		if !ok {
			return nil, telegrambot.NewUserError("选择的测速节点已离线或不可用，请重新选择。")
		}
		seen[id] = struct{}{}
		selected = append(selected, client)
	}
	if len(selected) == 0 {
		return nil, telegrambot.NewUserError("请至少选择一个测速节点。")
	}
	return selected, nil
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
var _ telegrambot.ProgressRunner = telegramTaskRunner{}
var _ telegrambot.ClientProvider = telegramTaskRunner{}
