package serverapp

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket/wsjson"

	"smalux-speedtest/internal/model"
	"smalux-speedtest/internal/store"
	"smalux-speedtest/internal/wire"
)

// TestDispatchPrecedesConcurrentCancel 人为占用 peer 写锁，把 Assignment 停在发送
// 途中，再并发取消。任务转换锁必须保证 Client 先收到 Assignment，
// 再收到 cancel，不能在终态后开始新测速。
func TestDispatchPrecedesConcurrentCancel(t *testing.T) {
	application, err := New(t.Context(), Config{
		Listen: ":0", DatabasePath: filepath.Join(t.TempDir(), "dispatch.db"), AdminPassword: "test-password",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.store.Close() })
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local sockets are unavailable: %v", err)
	}
	server := httptest.NewUnstartedServer(application.routes())
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	client, token, err := application.store.CreateClient(t.Context(), "dispatch-client", nil)
	if err != nil {
		t.Fatal(err)
	}
	connection, connected := dialTestClient(t, application, server.URL, client, token)
	connected.write.Lock()
	writeLocked := true
	defer func() {
		if writeLocked {
			connected.write.Unlock()
		}
	}()

	task := store.Task{ID: model.NewID(), Status: "queued", CandidateCount: 1, TopN: 1, Threads: 1, ProxyCount: 1, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err := application.store.CreateTask(t.Context(), task, []string{client.ID}); err != nil {
		t.Fatal(err)
	}
	addDone := make(chan struct{})
	go func() {
		application.hub.AddTask(testAssignment(task.ID), []string{client.ID})
		close(addDone)
	}()

	// dispatch 已取得 transition 锁且正等待 peer.write 时 TryLock 必须失败。
	deadline := time.Now().Add(2 * time.Second)
	for {
		application.hub.mu.RLock()
		runtime := application.hub.tasks[task.ID]
		application.hub.mu.RUnlock()
		if runtime != nil {
			if !runtime.transition.TryLock() {
				break
			}
			runtime.transition.Unlock()
		}
		if time.Now().After(deadline) {
			t.Fatal("dispatch did not reach blocked send")
		}
		time.Sleep(time.Millisecond)
	}
	cancelDone := make(chan error, 1)
	go func() { cancelDone <- application.hub.CancelTask(t.Context(), task.ID) }()
	select {
	case err := <-cancelDone:
		t.Fatalf("cancel bypassed in-flight assignment: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	connected.write.Unlock()
	writeLocked = false

	readCtx, readCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer readCancel()
	var first, second wire.Envelope
	if err := wsjson.Read(readCtx, connection, &first); err != nil {
		t.Fatal(err)
	}
	if err := wsjson.Read(readCtx, connection, &second); err != nil {
		t.Fatal(err)
	}
	if first.Type != wire.TypeTaskAssign || second.Type != wire.TypeTaskCancel {
		t.Fatalf("unexpected frame order: %s then %s", first.Type, second.Type)
	}
	if err := <-cancelDone; err != nil {
		t.Fatal(err)
	}
	<-addDone
}

// TestReplacedPeerCannotFinishTask 直接投递一条旧连接缓冲的 complete 帧，
// 确认连接指针身份校验会在任务状态转换前拒绝它。
func TestReplacedPeerCannotFinishTask(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "stale-peer.db"))
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
	oldPeer := &peer{client: client}
	current := &peer{client: client}
	hub.peers[client.ID] = current
	hub.tasks[task.ID] = &runtimeTask{assignment: testAssignment(task.ID), targets: map[string]string{client.ID: "assigned"}}
	complete, _ := wire.New(wire.TypeTaskComplete, task.ID, model.Ack{TaskID: task.ID})
	hub.handleMessage(t.Context(), oldPeer, complete)
	stored, err := database.GetTask(t.Context(), task.ID)
	if err != nil || stored.Status != "queued" {
		t.Fatalf("stale peer changed task: %+v %v", stored, err)
	}
	delete(hub.peers, client.ID)
	if err := hub.CancelTask(t.Context(), task.ID); err != nil {
		t.Fatal(err)
	}
}
