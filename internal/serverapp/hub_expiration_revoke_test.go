package serverapp

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"smalux-speedtest/internal/model"
	"smalux-speedtest/internal/store"
	"smalux-speedtest/internal/wire"
)

// TestExpireAssignedTaskNotifiesClientAndFailsDatabase 验证服务端整体超时不会只更新
// 管理页面：已经收到 Assignment 但尚未 ACK 的 Client 也必须收到 task.cancel，且
// SQLite 中父任务和目标必须在同一收敛结果中成为 failed。
func TestExpireAssignedTaskNotifiesClientAndFailsDatabase(t *testing.T) {
	application, server := newHubRegressionServer(t, "expire.db")
	client, token, err := application.store.CreateClient(t.Context(), "expire-client", nil)
	if err != nil {
		t.Fatal(err)
	}
	connection, _ := dialTestClient(t, application, server.URL, client, token)

	task := store.Task{
		ID: model.NewID(), Status: "queued", CandidateCount: 1, TopN: 1, Threads: 1,
		ProxyCount: 1, ClientCount: 1, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := application.store.CreateTask(t.Context(), task, []string{client.ID}); err != nil {
		t.Fatal(err)
	}
	application.hub.AddTask(testAssignment(task.ID), []string{client.ID})

	readCtx, readCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer readCancel()
	var assignment wire.Envelope
	if err := wsjson.Read(readCtx, connection, &assignment); err != nil || assignment.Type != wire.TypeTaskAssign {
		t.Fatalf("assignment = %+v, %v", assignment, err)
	}

	// 手动调用 expireTask 前停止十分钟定时器，避免测试结束后留下引用 Hub 的回调。
	application.hub.mu.RLock()
	runtimeTask := application.hub.tasks[task.ID]
	if runtimeTask == nil || runtimeTask.targets[client.ID] != "assigned" {
		application.hub.mu.RUnlock()
		t.Fatalf("target was not assigned before expiration: %+v", runtimeTask)
	}
	runtimeTask.expires.Stop()
	application.hub.mu.RUnlock()

	application.hub.expireTask(task.ID)
	var cancellation wire.Envelope
	if err := wsjson.Read(readCtx, connection, &cancellation); err != nil {
		t.Fatal(err)
	}
	if cancellation.Type != wire.TypeTaskCancel || cancellation.TaskID != task.ID {
		t.Fatalf("expiration frame = %+v, want task.cancel for %s", cancellation, task.ID)
	}

	stored, err := application.store.GetTask(t.Context(), task.ID)
	if err != nil || stored.Status != "failed" || stored.Error != "task expired after 10 minutes" {
		t.Fatalf("expired task = %+v, %v", stored, err)
	}
	completed, failed, canceled, total, err := application.store.TargetSummary(t.Context(), task.ID)
	if err != nil || completed != 0 || failed != 1 || canceled != 0 || total != 1 {
		t.Fatalf("expired targets = completed:%d failed:%d canceled:%d total:%d error:%v", completed, failed, canceled, total, err)
	}
	application.hub.mu.RLock()
	_, active := application.hub.tasks[task.ID]
	application.hub.mu.RUnlock()
	if active {
		t.Fatal("expired task remained active in Hub")
	}
}

// TestExpirePersistenceFailureStopsClientAndReschedules 关闭 Store 模拟即时落库失败。
// Client 仍须收到停止帧，runtime 中的代理配置则保留到下一次可持久化重试。
func TestExpirePersistenceFailureStopsClientAndReschedules(t *testing.T) {
	application, server := newHubRegressionServer(t, "expire-retry.db")
	client, token, err := application.store.CreateClient(t.Context(), "expire-retry-client", nil)
	if err != nil {
		t.Fatal(err)
	}
	connection, connected := dialTestClient(t, application, server.URL, client, token)
	task := store.Task{
		ID: model.NewID(), Status: "queued", CandidateCount: 1, TopN: 1, Threads: 1,
		ProxyCount: 1, ClientCount: 1, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := application.store.CreateTask(t.Context(), task, []string{client.ID}); err != nil {
		t.Fatal(err)
	}
	application.hub.AddTask(testAssignment(task.ID), []string{client.ID})
	readCtx, readCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer readCancel()
	var assignment wire.Envelope
	if err := wsjson.Read(readCtx, connection, &assignment); err != nil || assignment.Type != wire.TypeTaskAssign {
		t.Fatalf("assignment = %+v, %v", assignment, err)
	}
	application.hub.mu.RLock()
	runtime := application.hub.tasks[task.ID]
	oldTimer := runtime.expires
	oldTimer.Stop()
	application.hub.mu.RUnlock()
	if err := application.store.Close(); err != nil {
		t.Fatal(err)
	}

	application.hub.expireTask(task.ID)
	var cancellation wire.Envelope
	if err := wsjson.Read(readCtx, connection, &cancellation); err != nil || cancellation.Type != wire.TypeTaskCancel {
		t.Fatalf("expiration failure cancellation = %+v, %v", cancellation, err)
	}
	application.hub.mu.RLock()
	active := application.hub.tasks[task.ID]
	newTimer := runtime.expires
	application.hub.mu.RUnlock()
	if active != runtime || newTimer == nil || newTimer == oldTimer {
		t.Fatalf("expiry retry was not scheduled: active=%p runtime=%p old=%p new=%p", active, runtime, oldTimer, newTimer)
	}
	if !runtime.terminalPending {
		t.Fatal("expired runtime did not enter terminal-pending state")
	}

	// 模拟同一 Client 在终态重试窗口内断开并重连。Store 已关闭，因而直接调用 Hub
	// 的注册边界；这仍覆盖生产握手成功后的 unregister/register/dispatchQueued 顺序。
	application.hub.unregister(connected)
	reconnected := &peer{client: connected.client, conn: connected.conn}
	if !application.hub.register(reconnected) {
		t.Fatal("simulated reconnect was rejected")
	}
	application.hub.dispatchQueued(client.ID)
	noAssignmentCtx, noAssignmentCancel := context.WithTimeout(t.Context(), 80*time.Millisecond)
	defer noAssignmentCancel()
	var unexpected wire.Envelope
	if err := wsjson.Read(noAssignmentCtx, connection, &unexpected); err == nil {
		t.Fatalf("expired task was redispatched after reconnect: %+v", unexpected)
	}
	newTimer.Stop()
}

// TestClientRevocationLinearizesWithRegistration 覆盖撤销发生在注册线性化点两侧的行为：
// hello 前撤销必须阻止握手完成；已注册连接撤销后必须立即离线，且 tombstone 必须阻止
// 同一进程中的迟到 register 再次恢复该 Client。
func TestClientRevocationLinearizesWithRegistration(t *testing.T) {
	t.Run("revoked before hello", func(t *testing.T) {
		application, server := newHubRegressionServer(t, "revoke-before-hello.db")
		client, token, err := application.store.CreateClient(t.Context(), "pending-client", nil)
		if err != nil {
			t.Fatal(err)
		}
		connection := dialUnregisteredTestClient(t, server.URL, token)

		if err := application.store.RevokeClient(t.Context(), client.ID); err != nil {
			t.Fatal(err)
		}
		application.hub.RevokeClient(t.Context(), client.ID)
		hello, _ := wire.New(wire.TypeHello, "", model.Hello{Name: client.Name, Version: "test", OS: "linux", Arch: "amd64"})
		writeCtx, writeCancel := context.WithTimeout(t.Context(), 2*time.Second)
		err = wsjson.Write(writeCtx, connection, hello)
		writeCancel()
		if err != nil {
			t.Fatalf("write hello after revoke: %v", err)
		}

		readCtx, readCancel := context.WithTimeout(t.Context(), 2*time.Second)
		var response wire.Envelope
		err = wsjson.Read(readCtx, connection, &response)
		readCancel()
		if err == nil {
			t.Fatalf("revoked handshake received server frame: %+v", response)
		}
		if application.hub.Online(client.ID) {
			t.Fatal("client revoked before hello became online")
		}
	})

	t.Run("revoked after register", func(t *testing.T) {
		application, server := newHubRegressionServer(t, "revoke-after-register.db")
		client, token, err := application.store.CreateClient(t.Context(), "online-client", nil)
		if err != nil {
			t.Fatal(err)
		}
		connection, connected := dialTestClient(t, application, server.URL, client, token)
		if err := application.store.RevokeClient(t.Context(), client.ID); err != nil {
			t.Fatal(err)
		}
		revokeDone := make(chan struct{})
		go func() {
			application.hub.RevokeClient(context.Background(), client.ID)
			close(revokeDone)
		}()

		// 读取服务端 Close 帧并完成关闭握手，避免 RevokeClient 等待远端响应。
		readCtx, readCancel := context.WithTimeout(t.Context(), 3*time.Second)
		var response wire.Envelope
		_ = wsjson.Read(readCtx, connection, &response)
		readCancel()
		select {
		case <-revokeDone:
		case <-time.After(3 * time.Second):
			t.Fatal("RevokeClient did not finish closing the registered peer")
		}
		if application.hub.Online(client.ID) {
			t.Fatal("registered client remained online after revoke")
		}
		if application.hub.register(connected) {
			t.Fatal("revoked client tombstone allowed a late register")
		}
		if application.hub.Online(client.ID) {
			t.Fatal("late register restored a revoked client")
		}
	})
}

// TestAddTaskAfterClientRevocationClosesCreationWindow 固定 CreateTask 已提交、RevokeClient
// 已扫描完 Hub、AddTask 才登记运行态的顺序。tombstone 必须让刚加入的目标立即失败，
// 不能留下一个等待十分钟超时且永远无法重新连接的 queued 任务。
func TestAddTaskAfterClientRevocationClosesCreationWindow(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "revoked-add-task.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	client, _, err := database.CreateClient(t.Context(), "revoked-target", nil)
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

	hub := NewHub(database, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := database.RevokeClient(t.Context(), client.ID); err != nil {
		t.Fatal(err)
	}
	hub.RevokeClient(t.Context(), client.ID)
	// RevokeClient 此时没有可扫描的 runtimeTask；AddTask 必须自行识别 tombstone。
	hub.AddTask(testAssignment(task.ID), []string{client.ID})

	stored, err := database.GetTask(t.Context(), task.ID)
	if err != nil || stored.Status != "failed" {
		t.Fatalf("task added after revoke = %+v, %v", stored, err)
	}
	completed, failed, canceled, total, err := database.TargetSummary(t.Context(), task.ID)
	if err != nil || completed != 0 || failed != 1 || canceled != 0 || total != 1 {
		t.Fatalf("targets added after revoke = completed:%d failed:%d canceled:%d total:%d error:%v", completed, failed, canceled, total, err)
	}
	hub.mu.RLock()
	_, active := hub.tasks[task.ID]
	hub.mu.RUnlock()
	if active {
		t.Fatal("task added after revoke remained active in Hub")
	}
}

func newHubRegressionServer(t *testing.T, databaseName string) (*App, *httptest.Server) {
	t.Helper()
	application, err := New(t.Context(), Config{
		Listen: ":0", DatabasePath: filepath.Join(t.TempDir(), databaseName), AdminPassword: "test-password",
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
	return application, server
}

func dialUnregisteredTestClient(t *testing.T, baseURL, token string) *websocket.Conn {
	t.Helper()
	header := make(http.Header)
	header.Set("Authorization", "Bearer "+token)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	connection, _, err := websocket.Dial(ctx, "ws"+baseURL[len("http"):]+"/ws/client", &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.CloseNow() })
	return connection
}
