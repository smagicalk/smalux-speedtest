package serverapp

import (
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"smalux-speedtest/internal/model"
	"smalux-speedtest/internal/store"
)

// TestDisconnectDuringACKRequeuesCommittedRunningState 用第二条 SQLite 连接的写锁把
// StartTarget 停在“已校验旧 peer、尚未提交”的窗口，然后并发断线。
// ACK 提交后必须先把内存改为 running，等待 transition 锁的 unregister 才能
// 同时把 SQLite 和内存退回 queued，不能留下 DB=running/内存=assigned。
func TestDisconnectDuringACKRequeuesCommittedRunningState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ack-disconnect.db")
	database, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	client, _, err := database.CreateClient(t.Context(), "client", nil)
	if err != nil {
		t.Fatal(err)
	}
	task := store.Task{ID: model.NewID(), Status: "queued", CandidateCount: 1, TopN: 1, Threads: 1, ProxyCount: 1, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err := database.CreateTask(t.Context(), task, []string{client.ID}); err != nil {
		t.Fatal(err)
	}
	hub := NewHub(database, slog.New(slog.NewTextHandler(io.Discard, nil)))
	connected := &peer{client: client}
	assignment := testAssignment(task.ID)
	workID := model.NewID()
	ref := workRef{taskID: task.ID, clientID: client.ID, proxyID: assignment.Proxies[0].ID, workID: workID}
	key := hub.proxyWorkKey(assignment.Proxies[0].Outbound)
	runtime := &runtimeTask{
		assignment: assignment, targets: map[string]string{client.ID: "assigned"},
		work: map[string]*targetWork{client.ID: {order: []string{assignment.Proxies[0].ID}, completed: map[string]struct{}{}, active: &workLease{ref: ref, proxyKey: key, state: "assigned"}}},
	}
	hub.peers[client.ID] = connected
	hub.tasks[task.ID] = runtime
	hub.clientWork[client.ID] = ref
	hub.proxyWork[key] = ref

	blocker, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	if _, err := blocker.ExecContext(t.Context(), `PRAGMA busy_timeout=5000`); err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.ExecContext(t.Context(), `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	locked := true
	defer func() {
		if locked {
			_, _ = blocker.Exec(`ROLLBACK`)
		}
	}()

	ackDone := make(chan struct{})
	go func() {
		hub.setTargetRunning(t.Context(), task.ID, workID, connected)
		close(ackDone)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if !runtime.transition.TryLock() {
			break
		}
		runtime.transition.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("ACK did not acquire transition lock")
		}
		time.Sleep(time.Millisecond)
	}
	// StartTarget 已进入被外部写锁阻塞的 SQL 路径，留出断线交错窗口。
	time.Sleep(25 * time.Millisecond)
	unregisterDone := make(chan struct{})
	go func() {
		hub.unregister(connected)
		close(unregisterDone)
	}()
	for hub.Online(client.ID) {
		if time.Now().After(deadline) {
			t.Fatal("unregister did not remove peer")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := blocker.ExecContext(t.Context(), `ROLLBACK`); err != nil {
		t.Fatal(err)
	}
	locked = false
	select {
	case <-ackDone:
	case <-time.After(3 * time.Second):
		t.Fatal("ACK did not finish after releasing SQLite lock")
	}
	select {
	case <-unregisterDone:
	case <-time.After(3 * time.Second):
		t.Fatal("unregister did not finish after ACK")
	}
	hub.mu.RLock()
	memoryStatus := runtime.targets[client.ID]
	hub.mu.RUnlock()
	if memoryStatus != "queued" {
		t.Fatalf("memory target status = %q, want queued", memoryStatus)
	}
	// queued 是 StartTarget 的唯一可转换前置，成功返回同时验证 SQLite 已重排队。
	if started, err := database.StartTarget(t.Context(), task.ID, client.ID); err != nil || !started {
		t.Fatalf("database target was not requeued: started=%v err=%v", started, err)
	}
	if err := hub.CancelTask(t.Context(), task.ID); err != nil {
		t.Fatal(err)
	}
}
