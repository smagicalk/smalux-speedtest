package telegrambot

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

type scriptedPhotoAPI struct {
	mu       sync.Mutex
	errors   []error
	attempts int
}

func (a *scriptedPhotoAPI) GetUpdates(ctx context.Context, _ int64, _ time.Duration) ([]Update, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (*scriptedPhotoAPI) SendMessage(context.Context, int64, string) error { return nil }

func (a *scriptedPhotoAPI) SendPhoto(context.Context, int64, string, string, []byte) error {
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
		{name: "bad request", firstErr: &APIError{Method: "sendPhoto", StatusCode: 400}, want: 1, wantErr: true},
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
