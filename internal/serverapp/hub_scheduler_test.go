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

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"smalux-speedtest/internal/model"
	"smalux-speedtest/internal/store"
	"smalux-speedtest/internal/wire"
)

// TestSchedulerSerializesClientsAndProxyNodes verifies the full-matrix schedule:
// Clients run one work unit at a time, start on different proxies, and receive the
// next currently-unlocked proxy immediately after completing their active unit.
func TestSchedulerSerializesClientsAndProxyNodes(t *testing.T) {
	application, err := New(t.Context(), Config{
		Listen: ":0", DatabasePath: filepath.Join(t.TempDir(), "scheduler.db"), AdminPassword: "test-password",
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

	first, firstToken, err := application.store.CreateClient(t.Context(), "first", nil)
	if err != nil {
		t.Fatal(err)
	}
	second, secondToken, err := application.store.CreateClient(t.Context(), "second", nil)
	if err != nil {
		t.Fatal(err)
	}
	firstConnection, _ := dialTestClient(t, application, server.URL, first, firstToken)
	secondConnection, _ := dialTestClient(t, application, server.URL, second, secondToken)

	taskRecord := store.Task{
		ID: model.NewID(), Status: "queued", CandidateCount: 1, TopN: 1, Threads: 1,
		ProxyCount: 3, ClientCount: 2, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := application.store.CreateTask(t.Context(), taskRecord, []string{first.ID, second.ID}); err != nil {
		t.Fatal(err)
	}
	assignment := schedulerTestAssignment(taskRecord.ID)
	application.hub.AddTask(assignment, []string{first.ID, second.ID})

	firstWork := readAssignedWork(t, firstConnection)
	secondWork := readAssignedWork(t, secondConnection)
	if firstWork.WorkID == "" || secondWork.WorkID == "" || firstWork.WorkID == secondWork.WorkID {
		t.Fatalf("invalid work IDs: %q and %q", firstWork.WorkID, secondWork.WorkID)
	}
	if len(firstWork.Proxies) != 1 || len(secondWork.Proxies) != 1 || firstWork.Proxies[0].ID == secondWork.Proxies[0].ID {
		t.Fatalf("initial proxy assignments overlap: %+v %+v", firstWork.Proxies, secondWork.Proxies)
	}
	application.hub.mu.RLock()
	if len(application.hub.clientWork) != 2 || len(application.hub.proxyWork) != 2 {
		application.hub.mu.RUnlock()
		t.Fatalf("reserved clients=%d proxies=%d, want 2/2", len(application.hub.clientWork), len(application.hub.proxyWork))
	}
	application.hub.mu.RUnlock()

	ackWork(t, firstConnection, firstWork)
	ackWork(t, secondConnection, secondWork)
	completeAssignedWork(t, firstConnection, firstWork)
	firstNext := readAssignedWork(t, firstConnection)
	if firstNext.Proxies[0].ID == firstWork.Proxies[0].ID || firstNext.Proxies[0].ID == secondWork.Proxies[0].ID {
		t.Fatalf("first Client received locked or completed proxy %q", firstNext.Proxies[0].ID)
	}
	ackWork(t, firstConnection, firstNext)

	completeAssignedWork(t, secondConnection, secondWork)
	secondNext := readAssignedWork(t, secondConnection)
	if secondNext.Proxies[0].ID != firstWork.Proxies[0].ID {
		t.Fatalf("second Client next proxy=%q, want released %q", secondNext.Proxies[0].ID, firstWork.Proxies[0].ID)
	}
	if secondNext.Proxies[0].ID == firstNext.Proxies[0].ID {
		t.Fatal("both Clients received the same proxy concurrently")
	}

	if err := application.hub.CancelTask(t.Context(), taskRecord.ID); err != nil {
		t.Fatal(err)
	}
}

func schedulerTestAssignment(taskID string) model.Assignment {
	assignment := testAssignment(taskID)
	assignment.Proxies = []model.ProxySpec{
		{ID: "proxy-a", Name: "A", Protocol: "socks", Server: "a.example", Port: 1080, Outbound: []byte(`{"type":"socks","tag":"proxy","server":"a.example","server_port":1080}`)},
		{ID: "proxy-b", Name: "B", Protocol: "socks", Server: "b.example", Port: 1080, Outbound: []byte(`{"type":"socks","tag":"proxy","server":"b.example","server_port":1080}`)},
		{ID: "proxy-c", Name: "C", Protocol: "socks", Server: "c.example", Port: 1080, Outbound: []byte(`{"type":"socks","tag":"proxy","server":"c.example","server_port":1080}`)},
	}
	return assignment
}

func readAssignedWork(t *testing.T, connection *websocket.Conn) model.Assignment {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var envelope wire.Envelope
	if err := wsjson.Read(ctx, connection, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Type != wire.TypeTaskAssign {
		t.Fatalf("message type=%q, want %q", envelope.Type, wire.TypeTaskAssign)
	}
	assignment, err := wire.Decode[model.Assignment](envelope)
	if err != nil || len(assignment.Proxies) != 1 {
		t.Fatalf("invalid work assignment: %+v %v", assignment, err)
	}
	return assignment
}

func ackWork(t *testing.T, connection *websocket.Conn, assignment model.Assignment) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	message, _ := wire.New(wire.TypeTaskAck, assignment.TaskID, model.Ack{TaskID: assignment.TaskID, WorkID: assignment.WorkID})
	if err := wsjson.Write(ctx, connection, message); err != nil {
		t.Fatal(err)
	}
}

func completeAssignedWork(t *testing.T, connection *websocket.Conn, assignment model.Assignment) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	message, _ := wire.New(wire.TypeTaskComplete, assignment.TaskID, model.Ack{TaskID: assignment.TaskID, WorkID: assignment.WorkID})
	if err := wsjson.Write(ctx, connection, message); err != nil {
		t.Fatal(err)
	}
}
