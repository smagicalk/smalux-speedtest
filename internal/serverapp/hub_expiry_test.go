package serverapp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"smalux-speedtest/internal/model"
	"smalux-speedtest/internal/store"
)

// TestPendingExpiryCannotBeCanceledAndShutdownFlushesFailure 构造一次“超时决定已产生、
// SQLite 尚未提交”的重试窗口。普通取消不能覆盖该决定；Shutdown 停止 timer 后必须
// 主动把同一个 failed 终态写入父任务及所有未完成目标。
func TestPendingExpiryCannotBeCanceledAndShutdownFlushesFailure(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "pending-expiry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	hub := NewHub(database, slog.New(slog.NewTextHandler(io.Discard, nil)))
	client, _, err := database.CreateClient(t.Context(), "pending-expiry-client", nil)
	if err != nil {
		t.Fatal(err)
	}
	task := store.Task{
		ID: model.NewID(), Status: "queued", CandidateCount: 1, TopN: 1, Threads: 1,
		ProxyCount: 1, ClientCount: 1, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := database.CreateTask(t.Context(), task, []string{client.ID}); err != nil {
		t.Fatal(err)
	}
	hub.AddTask(testAssignment(task.ID), []string{client.ID})
	hub.mu.Lock()
	runtime := hub.tasks[task.ID]
	runtime.terminalPending = true
	hub.mu.Unlock()

	if err := hub.CancelTask(t.Context(), task.ID); !errors.Is(err, errTaskNotActive) {
		t.Fatalf("CancelTask error = %v, want terminal-pending rejection", err)
	}
	stored, err := database.GetTask(t.Context(), task.ID)
	if err != nil || stored.Status != "queued" {
		t.Fatalf("cancel overwrote pending expiry: task=%+v error=%v", stored, err)
	}
	if err := hub.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	stored, err = database.GetTask(t.Context(), task.ID)
	if err != nil || stored.Status != "failed" || stored.Error != expiredTaskDetail {
		t.Fatalf("shutdown did not flush pending expiry: task=%+v error=%v", stored, err)
	}
	completed, failed, canceled, total, err := database.TargetSummary(t.Context(), task.ID)
	if err != nil || completed != 0 || failed != 1 || canceled != 0 || total != 1 {
		t.Fatalf("flushed targets = completed:%d failed:%d canceled:%d total:%d error:%v", completed, failed, canceled, total, err)
	}
}
