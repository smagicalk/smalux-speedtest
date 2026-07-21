package telegrambot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// orderedDeliveryAPI 可以冻结一个会话的首条文本，用于验证同会话 FIFO 和跨会话并行。
// events 带缓冲，避免测试读取速度反过来影响 worker 的发送顺序。
type orderedDeliveryAPI struct {
	blockedStarted chan struct{}
	releaseBlocked chan struct{}
	events         chan string
	startOnce      sync.Once
}

func newOrderedDeliveryAPI() *orderedDeliveryAPI {
	return &orderedDeliveryAPI{
		blockedStarted: make(chan struct{}),
		releaseBlocked: make(chan struct{}),
		events:         make(chan string, 8),
	}
}

func (a *orderedDeliveryAPI) GetUpdates(ctx context.Context, _ int64, _ time.Duration) ([]Update, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (*orderedDeliveryAPI) SetMyCommands(context.Context, []BotCommand) error         { return nil }
func (*orderedDeliveryAPI) EditMessageText(context.Context, EditMessageRequest) error { return nil }
func (*orderedDeliveryAPI) AnswerCallbackQuery(context.Context, string, string) error { return nil }
func (*orderedDeliveryAPI) DeleteMessage(context.Context, int64, int64) error         { return nil }

func (a *orderedDeliveryAPI) SendMessage(ctx context.Context, request MessageRequest) (SentMessage, error) {
	if request.ChatID == 1 && request.Text == "created" {
		a.startOnce.Do(func() { close(a.blockedStarted) })
		select {
		case <-a.releaseBlocked:
		case <-ctx.Done():
			return SentMessage{}, ctx.Err()
		}
	}
	a.events <- fmt.Sprintf("text:%d:%s", request.ChatID, request.Text)
	return SentMessage{MessageID: 1}, nil
}

func (a *orderedDeliveryAPI) SendPhoto(_ context.Context, request PhotoRequest) error {
	a.events <- fmt.Sprintf("photo:%d:%s", request.ChatID, request.Filename)
	return nil
}

type scriptedPhotoAPI struct {
	mu       sync.Mutex
	errors   []error
	attempts int
}

func (a *scriptedPhotoAPI) GetUpdates(ctx context.Context, _ int64, _ time.Duration) ([]Update, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (*scriptedPhotoAPI) SetMyCommands(context.Context, []BotCommand) error         { return nil }
func (*scriptedPhotoAPI) EditMessageText(context.Context, EditMessageRequest) error { return nil }
func (*scriptedPhotoAPI) AnswerCallbackQuery(context.Context, string, string) error { return nil }
func (*scriptedPhotoAPI) DeleteMessage(context.Context, int64, int64) error         { return nil }
func (*scriptedPhotoAPI) SendMessage(context.Context, MessageRequest) (SentMessage, error) {
	return SentMessage{MessageID: 1}, nil
}

func (a *scriptedPhotoAPI) SendPhoto(context.Context, PhotoRequest) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	index := a.attempts
	a.attempts++
	if index < len(a.errors) {
		return a.errors[index]
	}
	return nil
}

// TestSendPhotoRetriesOnlyTransientFailures 验证 5xx 会有界重试，而 4xx 立即返回，
// 避免确定性错误占用 Bot 任务槽位。
func TestSendPhotoRetriesOnlyTransientFailures(t *testing.T) {
	for _, test := range []struct {
		name     string
		firstErr error
		want     int
		wantErr  bool
	}{
		{name: "server error", firstErr: &APIError{Method: "sendPhoto", StatusCode: 500}, want: 2},
		{name: "telegram rate limit in JSON", firstErr: &APIError{Method: "sendPhoto", StatusCode: 200, ErrorCode: 429}, want: 2},
		{name: "bad request", firstErr: &APIError{Method: "sendPhoto", StatusCode: 400}, want: 1, wantErr: true},
		{name: "telegram bad request in JSON", firstErr: &APIError{Method: "sendPhoto", StatusCode: 200, ErrorCode: 400}, want: 1, wantErr: true},
		{name: "network error", firstErr: errors.New("temporary network error"), want: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			api := &scriptedPhotoAPI{errors: []error{test.firstErr}}
			bot, err := New(api, newFakeAuthorization(9), &fakeRunner{}, &fakeRenderer{}, Config{
				RetryDelay: time.Millisecond, DeliveryAttempts: 3,
				Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
			})
			if err != nil {
				t.Fatal(err)
			}
			err = bot.sendPhotoWithRetry(t.Context(), 1, Image{Filename: "result.png", Data: []byte("PNG")})
			if (err != nil) != test.wantErr || api.attempts != test.want {
				t.Fatalf("sendPhoto = %v after %d attempts, want error=%v attempts=%d", err, api.attempts, test.wantErr, test.want)
			}
		})
	}
}

// TestOutboundDeliveryOrderedPerChat 验证文本和图片共享同一会话队列，同时一个慢会话
// 不会形成全局队头阻塞。这个顺序对“先确认任务、后返回结果图”的交互尤其重要。
func TestOutboundDeliveryOrderedPerChat(t *testing.T) {
	api := newOrderedDeliveryAPI()
	bot, err := New(api, newFakeAuthorization(9), &fakeRunner{}, &fakeRenderer{}, Config{
		RetryDelay: time.Millisecond,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}

	bot.sendText(t.Context(), 1, "created")
	select {
	case <-api.blockedStarted:
	case <-time.After(time.Second):
		t.Fatal("first chat worker did not start")
	}
	bot.sendText(t.Context(), 1, "second")
	photoDone := make(chan error, 1)
	photoCtx, cancelPhoto := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancelPhoto()
	go func() {
		photoDone <- bot.sendPhotoOrdered(photoCtx, 1, Image{Filename: "result.png", Data: []byte("PNG")})
	}()
	waitForDeliveryQueueLength(t, bot, 1, 2)

	// Chat 1 仍被冻结，但 Chat 2 应立即收到自己的回复。
	bot.sendText(t.Context(), 2, "parallel")
	select {
	case event := <-api.events:
		if event != "text:2:parallel" {
			t.Fatalf("unexpected event before releasing chat 1: %q", event)
		}
	case <-time.After(time.Second):
		t.Fatal("slow chat blocked another chat's delivery")
	}

	close(api.releaseBlocked)
	for _, want := range []string{"text:1:created", "text:1:second", "photo:1:result.png"} {
		select {
		case got := <-api.events:
			if got != want {
				t.Fatalf("delivery order = %q, want %q", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for %q", want)
		}
	}
	if err := <-photoDone; err != nil {
		t.Fatalf("ordered photo delivery failed: %v", err)
	}
	bot.deliveryWG.Wait()
}

func waitForDeliveryQueueLength(t *testing.T, bot *Bot, chatID int64, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		bot.deliveryMu.Lock()
		queued := len(bot.deliveryQueues[chatID])
		bot.deliveryMu.Unlock()
		if queued >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("chat %d delivery queue did not reach %d items", chatID, want)
}
