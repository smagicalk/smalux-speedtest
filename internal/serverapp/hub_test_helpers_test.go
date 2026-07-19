package serverapp

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"smalux-speedtest/internal/model"
	"smalux-speedtest/internal/store"
	"smalux-speedtest/internal/wire"
)

// dialTestClient 使用真实 WebSocket 完成 Bearer 认证和 hello/welcome 握手，
// 并等待 peer 在 Hub 中可见。返回的 peer 仅供同包并发回归测试控制写锁。
func dialTestClient(t *testing.T, application *App, baseURL string, client store.Client, token string) (*websocket.Conn, *peer) {
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
	hello, _ := wire.New(wire.TypeHello, "", model.Hello{Name: client.Name, Version: "test", OS: "linux", Arch: "amd64"})
	if err := wsjson.Write(ctx, connection, hello); err != nil {
		t.Fatal(err)
	}
	var welcome wire.Envelope
	if err := wsjson.Read(ctx, connection, &welcome); err != nil || welcome.Type != wire.TypeWelcome {
		t.Fatalf("invalid welcome: %+v %v", welcome, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		application.hub.mu.RLock()
		connected := application.hub.peers[client.ID]
		application.hub.mu.RUnlock()
		if connected != nil {
			return connection, connected
		}
		if time.Now().After(deadline) {
			t.Fatal("client peer was not registered")
		}
		time.Sleep(time.Millisecond)
	}
}

func testAssignment(taskID string) model.Assignment {
	return model.Assignment{
		TaskID: taskID, CandidateCount: 1, TopN: 1, Threads: 1, TimeoutSeconds: 60,
		Proxies: []model.ProxySpec{{
			ID: "proxy", Name: "node", Protocol: "socks", Server: "example.com", Port: 1080,
			Outbound: []byte(`{"type":"socks","tag":"proxy","server":"example.com","server_port":1080}`),
		}},
	}
}
