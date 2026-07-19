package serverapp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"smalux-speedtest/internal/model"
	"smalux-speedtest/internal/store"
	"smalux-speedtest/internal/wire"
)

func TestAdminLoginAndCreateClient(t *testing.T) {
	application, err := New(t.Context(), Config{
		Listen: ":0", DatabasePath: filepath.Join(t.TempDir(), "app.db"), AdminPassword: "test-password",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer application.store.Close()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local sockets are unavailable: %v", err)
	}
	server := httptest.NewUnstartedServer(application.routes())
	server.Listener = listener
	server.Start()
	defer server.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	response, err := client.PostForm(server.URL+"/login", url.Values{"password": {"test-password"}})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	response, err = client.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	match := regexp.MustCompile(`name="csrf-token" content="([^"]+)"`).FindSubmatch(body)
	if len(match) != 2 {
		t.Fatalf("CSRF token not found in dashboard: %s", body)
	}

	payload, _ := json.Marshal(map[string]any{"name": "test-client", "labels": map[string]string{"region": "test"}})
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/api/clients", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", string(match[1]))
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("create client returned %d: %s", response.StatusCode, body)
	}
	var result map[string]any
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result["token"] == "" {
		t.Fatal("client token missing")
	}
}

func TestWebSocketTaskLifecycle(t *testing.T) {
	application, err := New(t.Context(), Config{
		Listen: ":0", DatabasePath: filepath.Join(t.TempDir(), "ws.db"), AdminPassword: "test-password",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer application.store.Close()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local sockets are unavailable: %v", err)
	}
	server := httptest.NewUnstartedServer(application.routes())
	server.Listener = listener
	server.Start()
	defer server.Close()

	clientRecord, token, err := application.store.CreateClient(t.Context(), "ws-client", nil)
	if err != nil {
		t.Fatal(err)
	}
	header := make(http.Header)
	header.Set("Authorization", "Bearer "+token)
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/client"
	wsCtx, wsCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer wsCancel()
	connection, _, err := websocket.Dial(wsCtx, wsURL, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.CloseNow()
	hello, _ := wire.New(wire.TypeHello, "", model.Hello{Name: "ws-client", Version: "test", OS: "linux", Arch: "amd64"})
	if err := wsjson.Write(wsCtx, connection, hello); err != nil {
		t.Fatal(err)
	}
	var welcome wire.Envelope
	if err := wsjson.Read(wsCtx, connection, &welcome); err != nil || welcome.Type != wire.TypeWelcome {
		t.Fatalf("invalid welcome: %+v %v", welcome, err)
	}

	taskID := model.NewID()
	task := store.Task{ID: taskID, Status: "queued", CandidateCount: 1, TopN: 1, ProxyCount: 1, ClientCount: 1, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err := application.store.CreateTask(t.Context(), task, []string{clientRecord.ID}); err != nil {
		t.Fatal(err)
	}
	assignment := model.Assignment{TaskID: taskID, CandidateCount: 1, TopN: 1, TimeoutSeconds: 60, Proxies: []model.ProxySpec{{ID: "proxy", Name: "node", Protocol: "socks", Server: "example.com", Port: 1080, Outbound: json.RawMessage(`{"type":"socks","tag":"proxy","server":"example.com","server_port":1080}`)}}}
	application.hub.AddTask(assignment, []string{clientRecord.ID})
	var assigned wire.Envelope
	if err := wsjson.Read(wsCtx, connection, &assigned); err != nil || assigned.Type != wire.TypeTaskAssign {
		t.Fatalf("invalid assignment: %+v %v", assigned, err)
	}
	ack, _ := wire.New(wire.TypeTaskAck, taskID, model.Ack{TaskID: taskID})
	if err := wsjson.Write(wsCtx, connection, ack); err != nil {
		t.Fatal(err)
	}
	resultMessage, _ := wire.New(wire.TypeTaskResult, taskID, model.SpeedResult{
		TaskID: taskID, ProxyID: "proxy", ProxyName: "node", Protocol: "socks", MaskedAddress: "*.example.com:1080", SpeedServerID: "1", LatencyMS: 12,
	})
	if err := wsjson.Write(wsCtx, connection, resultMessage); err != nil {
		t.Fatal(err)
	}
	complete, _ := wire.New(wire.TypeTaskComplete, taskID, model.Ack{TaskID: taskID})
	if err := wsjson.Write(wsCtx, connection, complete); err != nil {
		t.Fatal(err)
	}

	deadline, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	for {
		stored, getErr := application.store.GetTask(deadline, taskID)
		if getErr == nil && stored.Status == "completed" {
			results, listErr := application.store.ListResults(deadline, taskID)
			if listErr != nil || len(results) != 1 || results[0].ClientID != clientRecord.ID {
				t.Fatalf("unexpected results: %+v %v", results, listErr)
			}
			break
		}
		select {
		case <-deadline.Done():
			t.Fatalf("task did not complete: %+v %v", stored, getErr)
		case <-time.After(10 * time.Millisecond):
		}
	}
}
