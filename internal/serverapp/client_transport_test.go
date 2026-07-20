package serverapp

import (
	"crypto/tls"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"smalux-speedtest/internal/store"
)

func TestServeWebSocketRequiresTLSForRemotePeers(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "transport.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	hub := NewHub(database, slog.New(slog.NewTextHandler(io.Discard, nil)))

	tests := []struct {
		name       string
		remoteAddr string
		tls        bool
		wantStatus int
	}{
		{name: "remote plaintext", remoteAddr: "198.51.100.10:1234", wantStatus: http.StatusUpgradeRequired},
		{name: "spoofed proxy header", remoteAddr: "198.51.100.10:1234", wantStatus: http.StatusUpgradeRequired},
		{name: "loopback plaintext", remoteAddr: "127.0.0.1:1234", wantStatus: http.StatusUnauthorized},
		{name: "remote TLS", remoteAddr: "198.51.100.10:1234", tls: true, wantStatus: http.StatusUnauthorized},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "http://server.test/ws/client", nil)
			request.RemoteAddr = test.remoteAddr
			request.Header.Set("X-Forwarded-Proto", "https")
			if test.tls {
				request.TLS = &tls.ConnectionState{}
			}
			response := httptest.NewRecorder()

			hub.ServeWebSocket(response, request)

			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
		})
	}
}
