package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"smalux-speedtest/internal/clientapp"
	"smalux-speedtest/internal/logsafe"
)

func TestResolveClientTokenPrefersEnvironment(t *testing.T) {
	t.Setenv("SMALUX_CLIENT_TOKEN", " environment-token ")
	if actual := resolveClientToken("legacy-flag-token"); actual != "environment-token" {
		t.Fatalf("resolved token = %q", actual)
	}

	t.Setenv("SMALUX_CLIENT_TOKEN", "")
	if actual := resolveClientToken(" legacy-flag-token "); actual != "legacy-flag-token" {
		t.Fatalf("legacy fallback token = %q", actual)
	}
}

// TestClientConfigurationLogOmitsToken guards the startup error path used by main.
// Configuration errors are intentionally logged by concrete type rather than message,
// so neither the environment token nor a future wrapped error can expose it.
func TestClientConfigurationLogOmitsToken(t *testing.T) {
	const secret = "client-token-should-never-enter-log"
	t.Setenv("SMALUX_CLIENT_TOKEN", secret)
	_, err := clientapp.New(clientapp.Config{ServerURL: "invalid", Token: resolveClientToken(""), Name: "node"})
	if err == nil {
		t.Fatal("invalid configuration was accepted")
	}
	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&output, nil))
	logger.Error("invalid configuration", "error_type", logsafe.ErrorType(err))
	if strings.Contains(output.String(), secret) || strings.Contains(err.Error(), secret) {
		t.Fatalf("configuration error exposed token: log=%q error=%q", output.String(), err)
	}
}
