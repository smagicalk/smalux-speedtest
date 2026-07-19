package telegrambot

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

// TestBotLongPollingAndTaskReplies 覆盖公开命令、私聊限制、授权、两类提交和图片回传。
func TestBotLongPollingAndTaskReplies(t *testing.T) {
	api := newFakeAPI([]Update{
		privateUpdate(1, 1, "/start"),
		privateUpdate(2, 2, "/id"),
		{UpdateID: 3, Message: &Message{From: &User{ID: 1}, Chat: Chat{ID: -100, Type: "group"}, Text: "ss://ignored"}},
		privateUpdate(4, 2, "ss://denied"),
		privateUpdate(5, 1, "/sub https://example.com/sub"),
		privateUpdate(6, 3, "http://user:pass@proxy.example:8080"),
		privateUpdate(7, 4, "/test ss://node"),
	})
	authorization := newFakeAuthorization(9, 1, 3, 4)
	runner := &fakeRunner{completion: Completion{Status: "completed"}}
	renderer := &fakeRenderer{image: Image{Filename: "result.png", Data: []byte("PNG")}}
	bot, err := New(api, authorization, runner, renderer, Config{
		PollTimeout: time.Second, RetryDelay: time.Millisecond, MaxConcurrentTasks: 4,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- bot.Run(ctx) }()
	for range 3 {
		select {
		case <-api.photoSignal:
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for result photos")
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v", err)
	}

	requests := runner.requestsSnapshot()
	if len(requests) != 3 {
		t.Fatalf("expected 3 task requests, got %+v", requests)
	}
	byUser := make(map[int64]TaskRequest, len(requests))
	for _, request := range requests {
		byUser[request.Principal.UserID] = request
	}
	if request := byUser[1]; request.SubscriptionURL != "https://example.com/sub" || request.Source != "" {
		t.Fatalf("unexpected subscription request: %+v", request)
	}
	if request := byUser[3]; request.Source != "http://user:pass@proxy.example:8080" || request.SubscriptionURL != "" {
		t.Fatalf("plain HTTP was not kept as source: %+v", request)
	}
	if request := byUser[4]; request.Source != "ss://node" {
		t.Fatalf("unexpected /test request: %+v", request)
	}
	messages, photos, offsets := api.snapshot()
	if len(photos) != 3 {
		t.Fatalf("expected 3 photos, got %+v", photos)
	}
	photoChats := make(map[int64]int, len(photos))
	for _, photo := range photos {
		photoChats[photo.chatID]++
	}
	for _, chatID := range []int64{1, 3, 4} {
		if photoChats[chatID] != 1 {
			t.Fatalf("result was not returned exactly once to originating chat %d: %+v", chatID, photos)
		}
	}
	if !containsMessage(messages, StartText) || !containsMessage(messages, "User ID：2") || !containsMessage(messages, "未获准") {
		t.Fatalf("missing expected public/denied replies: %+v", messages)
	}
	if containsChat(messages, -100) || len(offsets) < 2 || offsets[0] != 0 || offsets[1] != 8 {
		t.Fatalf("unexpected group reply or offsets: messages=%+v offsets=%v", messages, offsets)
	}
}
