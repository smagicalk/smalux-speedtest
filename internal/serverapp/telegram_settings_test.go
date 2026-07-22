package serverapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"smalux-speedtest/internal/store"
)

type telegramSettingsRoundTrip func(*http.Request) (*http.Response, error)

func (function telegramSettingsRoundTrip) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestOwnerCanVerifyBindAndToggleTelegramBot(t *testing.T) {
	const (
		token   = "123456:test-secret-token"
		ownerID = int64(99887766)
	)
	verificationCode := make(chan string, 1)
	client := &http.Client{Transport: telegramSettingsRoundTrip(func(request *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(request.URL.Path, "/getMe"):
			return telegramSettingsResponse(`{"ok":true,"result":{"id":123456,"is_bot":true,"username":"smalux_test_bot","first_name":"Test"}}`), nil
		case strings.HasSuffix(request.URL.Path, "/sendMessage"):
			body, _ := io.ReadAll(request.Body)
			match := regexp.MustCompile(`验证码：([0-9]{6})`).FindSubmatch(body)
			if len(match) == 2 {
				verificationCode <- string(match[1])
			}
			return telegramSettingsResponse(`{"ok":true,"result":{"message_id":1}}`), nil
		case strings.HasSuffix(request.URL.Path, "/setMyCommands"):
			return telegramSettingsResponse(`{"ok":true,"result":true}`), nil
		case strings.HasSuffix(request.URL.Path, "/getUpdates"):
			<-request.Context().Done()
			return nil, request.Context().Err()
		default:
			return &http.Response{StatusCode: http.StatusNotFound, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ok":false}`))}, nil
		}
	})}
	databasePath := filepath.Join(t.TempDir(), "telegram-settings.db")
	application, err := New(t.Context(), Config{
		DatabasePath: databasePath, AdminPassword: "test-password", TelegramHTTPClient: client,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = application.Shutdown(ctx)
	})
	admins, err := application.store.ListAdminUsers(t.Context())
	if err != nil || len(admins) != 1 || !admins[0].IsOwner {
		t.Fatalf("owner account = %+v, err=%v", admins, err)
	}
	ownerSession := application.sessions.create(admins[0].ID, admins[0].Username, true)

	verify := telegramSettingsRequest(t, application, ownerSession, http.MethodPost, "/api/settings/telegram/verify", map[string]any{"token": token, "owner_id": ownerID})
	if verify.Code != http.StatusOK {
		t.Fatalf("verify status=%d body=%s", verify.Code, verify.Body.String())
	}
	var challenge struct {
		ChallengeID string `json:"challenge_id"`
	}
	if err := json.Unmarshal(verify.Body.Bytes(), &challenge); err != nil || challenge.ChallengeID == "" {
		t.Fatalf("challenge response=%s err=%v", verify.Body.String(), err)
	}
	var code string
	select {
	case code = <-verificationCode:
	case <-time.After(time.Second):
		t.Fatal("verification code was not sent")
	}
	confirm := telegramSettingsRequest(t, application, ownerSession, http.MethodPost, "/api/settings/telegram/confirm", map[string]any{"challenge_id": challenge.ChallengeID, "code": code})
	if confirm.Code != http.StatusOK {
		t.Fatalf("confirm status=%d body=%s", confirm.Code, confirm.Body.String())
	}
	config, cipher, err := application.store.LoadTelegramConfig(t.Context())
	if err != nil || !config.Enabled || config.OwnerTelegramID != ownerID || bytes.Contains(cipher, []byte(token)) {
		t.Fatalf("persisted config=%+v plaintext=%t err=%v", config, bytes.Contains(cipher, []byte(token)), err)
	}
	key, err := application.telegramEncryptionKey()
	if err != nil {
		t.Fatal(err)
	}
	if plaintext, err := decryptTelegramToken(key, cipher); err != nil || plaintext != token {
		t.Fatalf("stored token decrypt=%q err=%v", plaintext, err)
	}

	operator, err := application.store.CreateAdminUser(t.Context(), "operator", "operator-password")
	if err != nil {
		t.Fatal(err)
	}
	operatorSession := application.sessions.create(operator.ID, operator.Username, false)
	forbidden := telegramSettingsRequest(t, application, operatorSession, http.MethodGet, "/api/settings/telegram", nil)
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("ordinary administrator settings status=%d", forbidden.Code)
	}

	disable := telegramSettingsRequest(t, application, ownerSession, http.MethodPatch, "/api/settings/telegram", map[string]any{"enabled": false})
	if disable.Code != http.StatusNoContent || application.telegramIsRunning() {
		t.Fatalf("disable status=%d running=%t body=%s", disable.Code, application.telegramIsRunning(), disable.Body.String())
	}
	disabledConfig, _, err := application.store.LoadTelegramConfig(t.Context())
	if err != nil || disabledConfig.Enabled {
		t.Fatalf("disabled config=%+v err=%v", disabledConfig, err)
	}
	enable := telegramSettingsRequest(t, application, ownerSession, http.MethodPatch, "/api/settings/telegram", map[string]any{"enabled": true})
	if enable.Code != http.StatusNoContent || !application.telegramIsRunning() {
		t.Fatalf("enable status=%d running=%t body=%s", enable.Code, application.telegramIsRunning(), enable.Body.String())
	}
	deleteResponse := telegramSettingsRequest(t, application, ownerSession, http.MethodDelete, "/api/settings/telegram", map[string]any{})
	if deleteResponse.Code != http.StatusNoContent || application.telegramIsRunning() {
		t.Fatalf("delete status=%d running=%t body=%s", deleteResponse.Code, application.telegramIsRunning(), deleteResponse.Body.String())
	}
	if _, _, err := application.store.LoadTelegramConfig(t.Context()); !errors.Is(err, store.ErrTelegramConfigMissing) {
		t.Fatalf("configuration still exists after unbind: %v", err)
	}
}

func telegramSettingsRequest(t *testing.T, application *App, session *session, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		payload = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, path, payload)
	request.AddCookie(&http.Cookie{Name: "smalux_session", Value: session.token})
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-CSRF-Token", session.csrf)
	}
	response := httptest.NewRecorder()
	application.routes().ServeHTTP(response, request)
	return response
}

func telegramSettingsResponse(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}
