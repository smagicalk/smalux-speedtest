package serverapp

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestTelegramEncryptedConfigurationRestarts verifies a web-managed configuration is
// decrypted on the next process start and begins polling without startup Bot flags.
func TestTelegramEncryptedConfigurationRestarts(t *testing.T) {
	const (
		token   = "444444:persisted-secret"
		ownerID = int64(123456789)
	)
	databasePath := filepath.Join(t.TempDir(), "telegram-restart.db")
	seedTelegramConfiguration(t, databasePath, token, ownerID)
	client := telegramPersistenceHTTPClient()
	application, err := New(t.Context(), Config{
		DatabasePath: databasePath, TelegramHTTPClient: client,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !application.telegramIsRunning() {
		t.Fatal("persisted enabled Bot did not start")
	}
	config, cipher, err := application.store.LoadTelegramConfig(t.Context())
	if err != nil || !config.Enabled || config.OwnerTelegramID != ownerID || bytes.Contains(cipher, []byte(token)) {
		t.Fatalf("restored config=%+v plaintext=%t err=%v", config, bytes.Contains(cipher, []byte(token)), err)
	}
	shutdownTelegramTestApp(t, application)
}

// TestTelegramMissingKeyDoesNotBlockServer verifies losing the local encryption key
// degrades only the optional Bot. The service remains available for an Owner rebind and
// logs neither the token nor the database path.
func TestTelegramMissingKeyDoesNotBlockServer(t *testing.T) {
	const token = "555555:unrecoverable-secret"
	databasePath := filepath.Join(t.TempDir(), "telegram-missing-key.db")
	seedTelegramConfiguration(t, databasePath, token, 9988)
	if err := os.Remove(databasePath + ".bot-key"); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	application, err := New(t.Context(), Config{
		DatabasePath: databasePath, TelegramHTTPClient: telegramPersistenceHTTPClient(),
		Logger: slog.New(slog.NewTextHandler(&logs, nil)),
	})
	if err != nil {
		t.Fatalf("server failed because Telegram key was missing: %v", err)
	}
	if application.telegramIsRunning() {
		t.Fatal("Bot started with an undecryptable token")
	}
	if strings.Contains(logs.String(), token) || strings.Contains(logs.String(), databasePath) {
		t.Fatalf("recovery log leaked secret or path: %s", logs.String())
	}
	shutdownTelegramTestApp(t, application)
}

func seedTelegramConfiguration(t *testing.T, databasePath, token string, ownerID int64) {
	t.Helper()
	application, err := New(t.Context(), Config{
		DatabasePath: databasePath, AdminPassword: "test-password",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	key, err := application.telegramEncryptionKey()
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := encryptTelegramToken(key, token)
	if err != nil {
		t.Fatal(err)
	}
	botID, err := telegramBotIDFromToken(token)
	if err != nil {
		t.Fatal(err)
	}
	if err := application.store.SaveTelegramConfig(t.Context(), cipher, botID, "persisted_bot", ownerID, true); err != nil {
		t.Fatal(err)
	}
	shutdownTelegramTestApp(t, application)
}

func telegramPersistenceHTTPClient() *http.Client {
	return &http.Client{Transport: telegramSettingsRoundTrip(func(request *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(request.URL.Path, "/setMyCommands"):
			return telegramSettingsResponse(`{"ok":true,"result":true}`), nil
		case strings.HasSuffix(request.URL.Path, "/getUpdates"):
			<-request.Context().Done()
			return nil, request.Context().Err()
		default:
			return telegramSettingsResponse(`{"ok":false,"error_code":404}`), nil
		}
	})}
}

func shutdownTelegramTestApp(t *testing.T, application *App) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := application.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}
