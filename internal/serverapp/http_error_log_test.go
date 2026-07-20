package serverapp

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestSanitizedHTTPErrorLogDropsStandardLibraryPayload(t *testing.T) {
	const secret = "198.51.100.22:43123 GET /private?token=secret"
	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))

	newSanitizedHTTPErrorLog(logger).Print(secret)

	logged := output.String()
	if strings.Contains(logged, secret) || strings.Contains(logged, "198.51.100.22") || strings.Contains(logged, "token=secret") {
		t.Fatalf("HTTP error payload leaked into log: %q", logged)
	}
	if !strings.Contains(logged, "http server error") {
		t.Fatalf("sanitized diagnostic event missing: %q", logged)
	}
}
