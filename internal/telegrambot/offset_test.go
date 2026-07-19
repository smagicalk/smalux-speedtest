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

type fakeOffsetStore struct {
	mu     sync.Mutex
	offset int64
	saved  []int64
}

func (s *fakeOffsetStore) LoadTelegramUpdateOffset(context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.offset, nil
}

func (s *fakeOffsetStore) SaveTelegramUpdateOffset(_ context.Context, offset int64) error {
	s.mu.Lock()
	s.offset = offset
	s.saved = append(s.saved, offset)
	s.mu.Unlock()
	return nil
}

// TestRunResumesPersistentOffset 验证重启后 Bot 跳过旧更新，并在处理新命令
// 前持久化下一个 offset。
func TestRunResumesPersistentOffset(t *testing.T) {
	api := newFakeAPI([]Update{
		privateUpdate(4, 1, "/id"),
		privateUpdate(5, 2, "/id"),
	})
	offsets := &fakeOffsetStore{offset: 5}
	bot, err := New(api, newFakeAuthorization(9), &fakeRunner{}, &fakeRenderer{}, Config{
		PollTimeout: time.Second, RetryDelay: time.Millisecond, UpdateOffsetStore: offsets,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- bot.Run(ctx) }()
	waitForMessage(t, api, "User ID：2")
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v", err)
	}
	messages, _, requestedOffsets := api.snapshot()
	if containsMessage(messages, "User ID：1") {
		t.Fatalf("persisted update was replayed: %+v", messages)
	}
	if len(requestedOffsets) == 0 || requestedOffsets[0] != 5 {
		t.Fatalf("getUpdates did not resume at 5: %v", requestedOffsets)
	}
	offsets.mu.Lock()
	defer offsets.mu.Unlock()
	if offsets.offset != 6 || len(offsets.saved) != 1 || offsets.saved[0] != 6 {
		t.Fatalf("saved offsets = %v, current=%d", offsets.saved, offsets.offset)
	}
}
