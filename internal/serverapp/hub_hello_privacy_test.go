package serverapp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"smalux-speedtest/internal/model"
	"smalux-speedtest/internal/wire"
)

// TestWebSocketHelloCannotReplaceManagedClientIdentity exercises the complete
// bearer-token handshake and the authenticated management response. A Client may
// report runtime metadata, but no Hello field may become a persistence side channel
// for an outbound or replace the administrator-owned name and labels.
func TestWebSocketHelloCannotReplaceManagedClientIdentity(t *testing.T) {
	application, err := New(t.Context(), Config{
		Listen: ":0", DatabasePath: filepath.Join(t.TempDir(), "hello-privacy.db"), AdminPassword: "test-password",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer application.store.Close()
	managedLabels := map[string]string{"region": "managed"}
	client, token, err := application.store.CreateClient(t.Context(), "managed-client", managedLabels)
	if err != nil {
		t.Fatal(err)
	}

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local sockets are unavailable: %v", err)
	}
	server := httptest.NewUnstartedServer(application.routes())
	server.Listener = listener
	server.Start()
	defer server.Close()

	header := make(http.Header)
	header.Set("Authorization", "Bearer "+token)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	connection, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/client", &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.CloseNow()

	const secret = "vless://uuid:password@secret.example:443/private/key"
	hello, _ := wire.New(wire.TypeHello, "", model.Hello{
		Name: secret, Labels: map[string]string{"outbound": secret},
		Version: secret, OS: secret, Arch: secret,
	})
	if err := wsjson.Write(ctx, connection, hello); err != nil {
		t.Fatal(err)
	}
	var welcome wire.Envelope
	if err := wsjson.Read(ctx, connection, &welcome); err != nil || welcome.Type != wire.TypeWelcome {
		t.Fatalf("invalid welcome: %+v, %v", welcome, err)
	}

	stored, err := application.store.ListClients(t.Context())
	if err != nil || len(stored) != 1 {
		t.Fatalf("stored clients = %+v, %v", stored, err)
	}
	if stored[0].Name != client.Name || stored[0].Labels["region"] != managedLabels["region"] || stored[0].Version != "" || stored[0].OS != "" || stored[0].Arch != "" {
		t.Fatalf("unsafe hello reached SQLite: %+v", stored[0])
	}
	encoded, _ := json.Marshal(stored[0])
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("SQLite client contains secret: %s", encoded)
	}

	admin, err := application.store.AuthenticateAdmin(t.Context(), "admin", "test-password")
	if err != nil {
		t.Fatal(err)
	}
	session := application.sessions.create(admin.ID, admin.Username)
	request, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/api/clients", nil)
	request.AddCookie(&http.Cookie{Name: "smalux_session", Value: session.token})
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("client API returned %d: %s", response.StatusCode, body)
	}
	if strings.Contains(string(body), secret) || !strings.Contains(string(body), client.Name) || !strings.Contains(string(body), managedLabels["region"]) {
		t.Fatalf("unsafe client API response: %s", body)
	}
}
