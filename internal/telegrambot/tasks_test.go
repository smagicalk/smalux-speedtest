package telegrambot

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// TestSingleActiveTaskAndCancel 验证同一用户不能重叠提交，且 /cancel 只取消其当前任务。
func TestSingleActiveTaskAndCancel(t *testing.T) {
	api := newFakeAPI(nil)
	authorization := newFakeAuthorization(9, 1)
	runner := &fakeRunner{wait: make(chan Completion, 1)}
	bot, err := New(api, authorization, runner, &fakeRenderer{}, Config{
		MaxConcurrentTasks: 2, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	bot.handleUpdate(t.Context(), privateUpdate(1, 1, "ss://first"))
	waitForMessage(t, api, "测速任务已创建：task-1")
	bot.handleUpdate(t.Context(), privateUpdate(2, 1, "ss://second"))
	if len(runner.requestsSnapshot()) != 1 {
		t.Fatalf("overlapping task was submitted: %+v", runner.requestsSnapshot())
	}
	bot.handleUpdate(t.Context(), privateUpdate(3, 1, "/cancel"))
	deadline := time.Now().Add(2 * time.Second)
	for {
		edits := api.editsSnapshot()
		if containsEdit(edits, "状态：canceled") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cancel completion was not reported: %+v", edits)
		}
		time.Sleep(time.Millisecond)
	}
	if canceled := runner.canceledSnapshot(); len(canceled) != 1 || canceled[0] != "task-1" {
		t.Fatalf("unexpected canceled tasks: %v", canceled)
	}
	messages, _, _ := api.snapshot()
	if !containsMessage(messages, "已有一个进行中的测速任务") || !containsMessage(messages, "已请求取消任务 task-1") {
		t.Fatalf("missing active/cancel replies: %+v", messages)
	}
	bot.wg.Wait()
}

func containsEdit(edits []EditMessageRequest, fragment string) bool {
	for _, edit := range edits {
		if strings.Contains(edit.Text, fragment) {
			return true
		}
	}
	return false
}

// TestSubmissionErrorVisibility 只回传显式 UserError，内部错误保持通用文案。
func TestSubmissionErrorVisibility(t *testing.T) {
	authorization := newFakeAuthorization(9, 1)
	for _, test := range []struct {
		name, want string
		err        error
	}{
		{name: "safe", err: NewUserError("当前没有在线 Client。"), want: "当前没有在线 Client。"},
		{name: "internal", err: errors.New("secret ss://credential"), want: "任务创建失败，请稍后重试"},
	} {
		t.Run(test.name, func(t *testing.T) {
			api := newFakeAPI(nil)
			runner := &fakeRunner{submitErr: test.err}
			var logs bytes.Buffer
			bot, err := New(api, authorization, runner, &fakeRenderer{}, Config{Logger: slog.New(slog.NewTextHandler(&logs, nil))})
			if err != nil {
				t.Fatal(err)
			}
			bot.handleUpdate(t.Context(), privateUpdate(1, 1, "ss://node"))
			waitForMessage(t, api, test.want)
			messages, _, _ := api.snapshot()
			if !containsMessage(messages, test.want) || containsMessage(messages, "credential") {
				t.Fatalf("unexpected submission error reply: %+v", messages)
			}
			if strings.Contains(logs.String(), "credential") || strings.Contains(logs.String(), "ss://") {
				t.Fatalf("submission error leaked source into logs: %s", logs.String())
			}
			bot.wg.Wait()
		})
	}
}

// TestSlowSubmissionDoesNotBlockCommands 验证订阅抓取等慢 Submit 在受限后台槽位中运行，
// getUpdates 循环仍能立即处理 /id、/cancel 或 owner 授权命令。
func TestSlowSubmissionDoesNotBlockCommands(t *testing.T) {
	api := newFakeAPI(nil)
	started := make(chan struct{})
	release := make(chan struct{})
	runner := &fakeRunner{submitStart: started, submitGate: release, completion: Completion{Status: "failed"}}
	bot, err := New(api, newFakeAuthorization(9, 1), runner, &fakeRenderer{}, Config{
		MaxConcurrentTasks: 1, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	bot.handleUpdate(t.Context(), privateUpdate(1, 1, "/sub https://example.com/sub"))
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("slow submission did not start")
	}
	bot.handleUpdate(t.Context(), privateUpdate(2, 1, "/id"))
	waitForMessage(t, api, "User ID：1")
	close(release)
	bot.wg.Wait()
}

// TestSlowCancelDoesNotBlockCommands 确认远程 Client 取消通知再慢也不会占住 Bot
// 的 getUpdates 处理循环。
func TestSlowCancelDoesNotBlockCommands(t *testing.T) {
	api := newFakeAPI(nil)
	cancelStarted := make(chan struct{})
	cancelRelease := make(chan struct{})
	runner := &fakeRunner{wait: make(chan Completion, 1), cancelStart: cancelStarted, cancelGate: cancelRelease}
	bot, err := New(api, newFakeAuthorization(9, 1), runner, &fakeRenderer{}, Config{
		MaxConcurrentTasks: 1, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	bot.handleUpdate(t.Context(), privateUpdate(1, 1, "ss://node"))
	waitForMessage(t, api, "测速任务已创建：task-1")
	bot.handleUpdate(t.Context(), privateUpdate(2, 1, "/cancel"))
	select {
	case <-cancelStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("slow cancellation did not start")
	}
	// 取消尚未返回时重复请求必须只得到状态回复，不能再启动 goroutine。
	for updateID := int64(3); updateID < 8; updateID++ {
		bot.handleUpdate(t.Context(), privateUpdate(updateID, 1, "/cancel"))
	}
	waitForMessage(t, api, "正在取消")
	if calls := runner.cancelCallCount(); calls != 1 {
		t.Fatalf("Cancel calls while in flight = %d, want 1", calls)
	}
	bot.handleUpdate(t.Context(), privateUpdate(8, 1, "/id"))
	waitForMessage(t, api, "User ID：1")
	close(cancelRelease)
	bot.wg.Wait()
}

// TestFailedTaskReturnsReport 按“测试结束即回图”处理全部失败的任务；
// Renderer 可生成带任务参数和空结果行的诊断图。
func TestFailedTaskReturnsReport(t *testing.T) {
	api := newFakeAPI(nil)
	runner := &fakeRunner{completion: Completion{Status: "failed"}}
	renderer := &fakeRenderer{image: Image{Filename: "failed.png", Data: []byte("PNG")}}
	bot, err := New(api, newFakeAuthorization(9, 1), runner, renderer, Config{
		RetryDelay: time.Millisecond, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	bot.handleUpdate(t.Context(), privateUpdate(1, 1, "ss://node"))
	select {
	case <-api.photoSignal:
	case <-time.After(2 * time.Second):
		t.Fatal("failed task did not return a report image")
	}
	bot.wg.Wait()
	_, photos, _ := api.snapshot()
	if len(photos) != 1 || photos[0].chatID != 1 || !strings.Contains(photos[0].caption, "failed") {
		t.Fatalf("unexpected failed report delivery: %+v", photos)
	}
}
