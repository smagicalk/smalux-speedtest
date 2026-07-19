package serverapp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket/wsjson"

	"smalux-speedtest/internal/model"
	"smalux-speedtest/internal/store"
	"smalux-speedtest/internal/wire"
)

// TestAppShutdownClosesWebSocketBeforeStore 以真实 http.Server 和 WebSocket 验证关闭顺序：
// 长连接被主动打断、handler 退出、assigned 任务落库取消，最后 SQLite 才关闭。
func TestAppShutdownClosesWebSocketBeforeStore(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "shutdown.db")
	application, err := New(t.Context(), Config{
		Listen: ":0", DatabasePath: databasePath, AdminPassword: "test-password",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			application.hub.BeginShutdown()
			_ = application.server.Close()
			_ = application.store.Close()
		}
	}()
	client, token, err := application.store.CreateClient(t.Context(), "shutdown-client", nil)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local sockets are unavailable: %v", err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- application.server.Serve(listener) }()
	baseURL := "http://" + listener.Addr().String()
	connection, _ := dialTestClient(t, application, baseURL, client, token)
	task := store.Task{ID: model.NewID(), Status: "queued", CandidateCount: 1, TopN: 1, Threads: 1, ProxyCount: 1, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err := application.store.CreateTask(t.Context(), task, []string{client.ID}); err != nil {
		t.Fatal(err)
	}
	application.hub.AddTask(testAssignment(task.ID), []string{client.ID})
	readCtx, readCancel := context.WithTimeout(t.Context(), 3*time.Second)
	var assignment wire.Envelope
	if err := wsjson.Read(readCtx, connection, &assignment); err != nil || assignment.Type != wire.TypeTaskAssign {
		readCancel()
		t.Fatalf("assignment was not delivered: %+v %v", assignment, err)
	}
	readCancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(t.Context(), 5*time.Second)
	err = application.Shutdown(shutdownCtx)
	shutdownCancel()
	if err != nil {
		t.Fatal(err)
	}
	closed = true
	if serveErr := <-serveDone; !errors.Is(serveErr, http.ErrServerClosed) {
		t.Fatalf("Serve returned %v", serveErr)
	}

	// 重新打开同一文件验证 Shutdown 在关库前已提交任务和目标终态。
	reopened, err := store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	stored, err := reopened.GetTask(t.Context(), task.ID)
	if err != nil || stored.Status != "canceled" {
		t.Fatalf("shutdown task = %+v, %v", stored, err)
	}
	completed, failed, canceled, total, err := reopened.TargetSummary(t.Context(), task.ID)
	if err != nil || completed != 0 || failed != 0 || canceled != 1 || total != 1 {
		t.Fatalf("shutdown targets = completed:%d failed:%d canceled:%d total:%d error:%v", completed, failed, canceled, total, err)
	}
}

// TestCancelTaskHonorsOverallDeadline 锁住任务转换点模拟正在发送的 Assignment。
// cancelTask 必须在调用方总 deadline 到期后返回，不能为每个等待阶段重新获得五秒。
func TestCancelTaskHonorsOverallDeadline(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "shutdown-deadline.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	hub := NewHub(database, slog.New(slog.NewTextHandler(io.Discard, nil)))
	task := &runtimeTask{targets: map[string]string{}}
	task.transition.Lock()
	defer task.transition.Unlock()
	hub.tasks["blocked"] = task

	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Millisecond)
	defer cancel()
	started := time.Now()
	err = hub.cancelTask(ctx, "blocked", "shutdown test")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelTask error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("cancelTask exceeded overall deadline: %v", elapsed)
	}
}

// TestCancelTaskHonorsExplicitCancellation 覆盖没有 Deadline 的手动 cancel。等待任务锁
// 时尚未开始持久化事务，因此 cancelTask 必须立即返回，而不能继续等待内部五秒上限。
func TestCancelTaskHonorsExplicitCancellation(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "shutdown-cancel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	hub := NewHub(database, slog.New(slog.NewTextHandler(io.Discard, nil)))
	task := &runtimeTask{targets: map[string]string{}}
	task.transition.Lock()
	defer task.transition.Unlock()
	hub.tasks["blocked"] = task

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	started := time.Now()
	err = hub.cancelTask(ctx, "blocked", "explicit cancellation test")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelTask error = %v, want context canceled", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("cancelTask ignored explicit cancellation: %v", elapsed)
	}
}
