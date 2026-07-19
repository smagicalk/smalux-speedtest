package store

import (
	"path/filepath"
	"testing"
)

// TestTelegramUpdateOffsetIsPersistentAndMonotonic 确认 Bot 游标跨 Store 重开保留，
// 并且迟到的较小 offset 不会导致历史高流量任务被重放。
func TestTelegramUpdateOffsetIsPersistentAndMonotonic(t *testing.T) {
	const (
		firstBotID  int64 = 123456
		secondBotID int64 = 654321
	)
	path := filepath.Join(t.TempDir(), "telegram-offset.db")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if offset, err := database.LoadTelegramUpdateOffset(t.Context(), firstBotID); err != nil || offset != 0 {
		t.Fatalf("initial offset = %d, %v", offset, err)
	}
	if err := database.SaveTelegramUpdateOffset(t.Context(), firstBotID, 42); err != nil {
		t.Fatal(err)
	}
	if err := database.SaveTelegramUpdateOffset(t.Context(), firstBotID, 7); err != nil {
		t.Fatal(err)
	}
	// 同一个 SQLite 可由后续配置的另一个 Bot 复用，但两个更新流的 offset
	// 完全无关。新 Bot 必须从 0 开始，而不是继承 42。
	if offset, err := database.LoadTelegramUpdateOffset(t.Context(), secondBotID); err != nil || offset != 0 {
		t.Fatalf("second bot initial offset = %d, %v; want 0", offset, err)
	}
	if err := database.SaveTelegramUpdateOffset(t.Context(), secondBotID, 9); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if offset, err := reopened.LoadTelegramUpdateOffset(t.Context(), firstBotID); err != nil || offset != 42 {
		t.Fatalf("reopened offset = %d, %v", offset, err)
	}
	if offset, err := reopened.LoadTelegramUpdateOffset(t.Context(), secondBotID); err != nil || offset != 9 {
		t.Fatalf("reopened second bot offset = %d, %v", offset, err)
	}
	if err := reopened.SaveTelegramUpdateOffset(t.Context(), firstBotID, -1); err == nil {
		t.Fatal("negative offset was accepted")
	}
	if _, err := reopened.LoadTelegramUpdateOffset(t.Context(), 0); err == nil {
		t.Fatal("non-positive bot ID was accepted")
	}
}
