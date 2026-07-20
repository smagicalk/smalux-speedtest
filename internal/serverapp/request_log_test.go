package serverapp

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRequestLogUsesRoutePattern confirms that both matched path values and unknown
// paths remain outside logs. Reverse proxies commonly preserve application logs for
// longer than request data, so an attacker-controlled URL must not become a secret
// retention channel.
func TestRequestLogUsesRoutePattern(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))
	application := &App{config: Config{Logger: logger}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/tasks/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	handler := application.logRequests(mux)

	const secret = "credential-should-never-enter-request-log"
	for _, path := range []string{"/api/tasks/" + secret, "/unknown/" + secret} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
	}
	logs, err := io.ReadAll(&output)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logs), secret) {
		t.Fatalf("request log exposed path value: %s", logs)
	}
	if !strings.Contains(string(logs), "GET /api/tasks/{id}") || !strings.Contains(string(logs), "unmatched") {
		t.Fatalf("request log omitted safe route labels: %s", logs)
	}
}
