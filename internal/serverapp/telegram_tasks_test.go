package serverapp

import (
	"context"
	"errors"
	"testing"
	"time"

	"smalux-speedtest/internal/model"
	"smalux-speedtest/internal/store"
	"smalux-speedtest/internal/telegrambot"
)

// TestTelegramTaskSubmitWithoutOnlineClient 验证 Bot 没有可用执行节点时返回显式 UserError，
// 而不是创建会永久停留在 queued 的任务或向用户暴露内部错误。
func TestTelegramTaskSubmitWithoutOnlineClient(t *testing.T) {
	application := newTaskServiceTestApp(t)
	// 即使数据库中存在已启用 Client，只要 Hub 没有在线连接也不能成为 Bot 任务目标。
	if _, _, err := application.store.CreateClient(t.Context(), "offline", nil); err != nil {
		t.Fatal(err)
	}
	runner := telegramTaskRunner{app: application}
	_, err := runner.Submit(t.Context(), telegrambot.TaskRequest{Source: "socks5://example.com:1080#node"})
	var userError telegrambot.UserError
	if !errors.As(err, &userError) {
		t.Fatalf("Submit error = %v, want telegrambot.UserError", err)
	}
	if message := telegrambot.UserMessage(err); message != "当前没有在线且已启用的 Client。" {
		t.Fatalf("UserMessage = %q", message)
	}
	tasks, listErr := application.store.ListTasks(t.Context(), 10)
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(tasks) != 0 {
		t.Fatalf("Submit persisted tasks without an online Client: %+v", tasks)
	}
}

// TestTelegramTaskSubmitSelectsEnabledOnlineClients 同时构造在线/离线、启用/撤销组合，
// 确认 Bot 只把 enabled+online 的交集交给共用任务服务，并成功创建可取消的任务。
func TestTelegramTaskSubmitSelectsEnabledOnlineClients(t *testing.T) {
	application := newTaskServiceTestApp(t)
	onlineA, _, err := application.store.CreateClient(t.Context(), "a-online", nil)
	if err != nil {
		t.Fatal(err)
	}
	offline, _, err := application.store.CreateClient(t.Context(), "b-offline", nil)
	if err != nil {
		t.Fatal(err)
	}
	revokedOnline, _, err := application.store.CreateClient(t.Context(), "c-revoked-online", nil)
	if err != nil {
		t.Fatal(err)
	}
	onlineD, _, err := application.store.CreateClient(t.Context(), "d-online", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := application.store.RevokeClient(t.Context(), revokedOnline.ID); err != nil {
		t.Fatal(err)
	}

	// Online 只判断 peers 中是否存在 ID。nil peer 足以构造在线快照；dispatch 会在发现
	// 连接为 nil 时直接返回，因此测试不会进行 WebSocket I/O。
	application.hub.mu.Lock()
	application.hub.peers[onlineA.ID] = nil
	application.hub.peers[revokedOnline.ID] = nil
	application.hub.peers[onlineD.ID] = nil
	application.hub.mu.Unlock()
	runner := telegramTaskRunner{app: application}
	created, err := runner.Submit(t.Context(), telegrambot.TaskRequest{Source: "socks5://example.com:1080#node"})
	if err != nil {
		t.Fatal(err)
	}
	if created.ID == "" {
		t.Fatal("Submit returned an empty task ID")
	}
	// Cancel 同时清除 Hub 中的敏感 Assignment 并停止 10 分钟到期 timer。
	defer func() {
		if err := runner.Cancel(t.Context(), created.ID); err != nil {
			t.Errorf("cancel Telegram task: %v", err)
		}
	}()

	stored, err := application.store.GetTask(t.Context(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ClientCount != 2 || stored.Status != "queued" || stored.CandidateCount != 10 || stored.TopN != 3 || stored.Threads != 4 {
		t.Fatalf("unexpected stored Telegram task: %+v", stored)
	}
	application.hub.mu.RLock()
	runtime := application.hub.tasks[created.ID]
	if runtime == nil {
		application.hub.mu.RUnlock()
		t.Fatal("Telegram task was not registered in Hub")
	}
	_, hasOnlineA := runtime.targets[onlineA.ID]
	_, hasOffline := runtime.targets[offline.ID]
	_, hasRevoked := runtime.targets[revokedOnline.ID]
	_, hasOnlineD := runtime.targets[onlineD.ID]
	targetCount := len(runtime.targets)
	application.hub.mu.RUnlock()
	if targetCount != 2 || !hasOnlineA || !hasOnlineD || hasOffline || hasRevoked {
		t.Fatalf("unexpected Telegram targets: count=%d onlineA=%v offline=%v revoked=%v onlineD=%v",
			targetCount, hasOnlineA, hasOffline, hasRevoked, hasOnlineD)
	}
}

// TestTelegramTaskWaitReturnsCompletedSnapshot 预置一个终态任务及已脱敏结果，验证 Wait
// 无需等待事件或轮询即可返回完整 Task/Results payload，并在返回时注销 Hub 订阅。
func TestTelegramTaskWaitReturnsCompletedSnapshot(t *testing.T) {
	application := newTaskServiceTestApp(t)
	client, _, err := application.store.CreateClient(t.Context(), "result-client", nil)
	if err != nil {
		t.Fatal(err)
	}
	task := store.Task{
		ID: model.NewID(), Status: "queued", CandidateCount: 10, TopN: 3, Threads: 4,
		ProxyCount: 1, ClientCount: 1, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := application.store.CreateTask(t.Context(), task, []string{client.ID}); err != nil {
		t.Fatal(err)
	}
	if err := application.store.SaveResult(t.Context(), model.SpeedResult{
		TaskID: task.ID, ClientID: client.ID, ProxyID: "proxy-1", ProxyName: "node", Protocol: "vless",
		MaskedAddress: "*.example.com:443", SpeedServerID: "server-1", SpeedServerName: "Tokyo",
		LatencyMS: 42.5, DownloadBPS: 800_000_000, UploadBPS: 200_000_000,
	}); err != nil {
		t.Fatal(err)
	}
	if err := application.store.SetTaskStatus(t.Context(), task.ID, "completed", "finished"); err != nil {
		t.Fatal(err)
	}

	runner := telegramTaskRunner{app: application}
	waitCtx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	completion, err := runner.Wait(waitCtx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completion.TaskID != task.ID || completion.Status != "completed" || completion.Message != "finished" {
		t.Fatalf("unexpected completion: %+v", completion)
	}
	payload, ok := completion.Payload.(telegramTaskPayload)
	if !ok {
		t.Fatalf("payload type = %T, want telegramTaskPayload", completion.Payload)
	}
	if payload.Task.ID != task.ID || payload.Task.Status != "completed" || len(payload.Results) != 1 {
		t.Fatalf("unexpected completion payload: %+v", payload)
	}
	result := payload.Results[0]
	if result.ClientID != client.ID || result.ClientName != client.Name || result.MaskedAddress != "*.example.com:443" {
		t.Fatalf("unexpected persisted result payload: %+v", result)
	}
	application.hub.mu.RLock()
	_, subscribed := application.hub.subscribers[task.ID]
	application.hub.mu.RUnlock()
	if subscribed {
		t.Fatal("Wait leaked a Hub subscriber after immediate completion")
	}
}
