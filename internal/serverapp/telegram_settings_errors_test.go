package serverapp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestTelegramSettingsRequireOwner verifies both the rendered settings page and every
// Bot control endpoint reject an otherwise valid ordinary administrator session.
func TestTelegramSettingsRequireOwner(t *testing.T) {
	application, ownerSession := newTelegramSettingsTestApp(t, nil)
	ownerPage := telegramSettingsRequest(t, application, ownerSession, http.MethodGet, "/settings", nil)
	if ownerPage.Code != http.StatusOK || !strings.Contains(ownerPage.Body.String(), "Telegram Bot") || !strings.Contains(ownerPage.Body.String(), "管理员账户") {
		t.Fatalf("owner settings page status=%d body=%s", ownerPage.Code, ownerPage.Body.String())
	}
	operator, err := application.store.CreateAdminUser(t.Context(), "operator", "operator-password")
	if err != nil {
		t.Fatal(err)
	}
	operatorSession := application.sessions.create(operator.ID, operator.Username, false)
	tests := []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodGet, "/settings", nil},
		{http.MethodGet, "/api/settings/telegram", nil},
		{http.MethodPost, "/api/settings/telegram/verify", map[string]any{"token": "123456:test", "owner_id": 1}},
		{http.MethodPost, "/api/settings/telegram/confirm", map[string]any{"challenge_id": "x", "code": "123456"}},
		{http.MethodPatch, "/api/settings/telegram", map[string]any{"enabled": true}},
		{http.MethodDelete, "/api/settings/telegram", map[string]any{}},
	}
	for _, test := range tests {
		response := telegramSettingsRequest(t, application, operatorSession, test.method, test.path, test.body)
		if response.Code != http.StatusForbidden {
			t.Errorf("%s %s status=%d, want 403", test.method, test.path, response.Code)
		}
	}
}

// TestTelegramBindingRejectsInvalidIdentityAndDelivery covers failures before a
// challenge is retained. The transport behavior is selected by the synthetic Bot ID.
func TestTelegramBindingRejectsInvalidIdentityAndDelivery(t *testing.T) {
	client := &http.Client{Transport: telegramSettingsRoundTrip(func(request *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(request.URL.Path, "/bot111111:") && strings.HasSuffix(request.URL.Path, "/getMe"):
			return telegramSettingsResponse(`{"ok":true,"result":{"id":999999,"is_bot":true,"username":"wrong_bot"}}`), nil
		case strings.Contains(request.URL.Path, "/bot222222:") && strings.HasSuffix(request.URL.Path, "/getMe"):
			return telegramSettingsResponse(`{"ok":true,"result":{"id":222222,"is_bot":true,"username":"unreachable_bot"}}`), nil
		case strings.Contains(request.URL.Path, "/bot222222:") && strings.HasSuffix(request.URL.Path, "/sendMessage"):
			return telegramSettingsResponse(`{"ok":false,"error_code":403,"description":"Forbidden"}`), nil
		default:
			return telegramSettingsResponse(`{"ok":false,"error_code":404,"description":"Not Found"}`), nil
		}
	})}
	application, ownerSession := newTelegramSettingsTestApp(t, client)
	tests := []struct {
		name    string
		payload map[string]any
	}{
		{name: "malformed token", payload: map[string]any{"token": "not-a-token", "owner_id": 1}},
		{name: "missing owner", payload: map[string]any{"token": "111111:test", "owner_id": 0}},
		{name: "getMe identity mismatch", payload: map[string]any{"token": "111111:test", "owner_id": 1}},
		{name: "verification message rejected", payload: map[string]any{"token": "222222:test", "owner_id": 1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := telegramSettingsRequest(t, application, ownerSession, http.MethodPost, "/api/settings/telegram/verify", test.payload)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
	application.telegramMu.Lock()
	pending := len(application.telegramBindings)
	application.telegramMu.Unlock()
	if pending != 0 {
		t.Fatalf("failed verification retained %d challenges", pending)
	}
}

// TestTelegramBindingRejectsWrongAndExpiredCodes confirms an incorrect code cannot
// persist configuration and that an expired challenge is removed even with the right code.
func TestTelegramBindingRejectsWrongAndExpiredCodes(t *testing.T) {
	const token = "333333:challenge-secret"
	var code string
	client := &http.Client{Transport: telegramSettingsRoundTrip(func(request *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(request.URL.Path, "/getMe"):
			return telegramSettingsResponse(`{"ok":true,"result":{"id":333333,"is_bot":true,"username":"challenge_bot"}}`), nil
		case strings.HasSuffix(request.URL.Path, "/sendMessage"):
			var body struct {
				Text string `json:"text"`
			}
			_ = json.NewDecoder(request.Body).Decode(&body)
			parts := strings.Split(body.Text, "：")
			if len(parts) > 1 && len(parts[1]) >= 6 {
				code = parts[1][:6]
			}
			return telegramSettingsResponse(`{"ok":true,"result":{"message_id":1}}`), nil
		default:
			return telegramSettingsResponse(`{"ok":false,"error_code":404}`), nil
		}
	})}
	application, ownerSession := newTelegramSettingsTestApp(t, client)
	verify := telegramSettingsRequest(t, application, ownerSession, http.MethodPost, "/api/settings/telegram/verify", map[string]any{"token": token, "owner_id": 77})
	if verify.Code != http.StatusOK || code == "" {
		t.Fatalf("verify status=%d code=%q body=%s", verify.Code, code, verify.Body.String())
	}
	var challenge struct {
		ID string `json:"challenge_id"`
	}
	if err := json.Unmarshal(verify.Body.Bytes(), &challenge); err != nil || challenge.ID == "" {
		t.Fatalf("challenge response=%s err=%v", verify.Body.String(), err)
	}
	wrongCode := "000000"
	if code == wrongCode {
		wrongCode = "999999"
	}
	wrong := telegramSettingsRequest(t, application, ownerSession, http.MethodPost, "/api/settings/telegram/confirm", map[string]any{"challenge_id": challenge.ID, "code": wrongCode})
	if wrong.Code != http.StatusBadRequest {
		t.Fatalf("wrong code status=%d body=%s", wrong.Code, wrong.Body.String())
	}
	application.telegramMu.Lock()
	retained := application.telegramBindings[challenge.ID]
	if retained != nil {
		retained.expiresAt = time.Now().Add(-time.Second)
	}
	application.telegramMu.Unlock()
	if retained == nil || retained.attempts != 4 {
		t.Fatalf("wrong code challenge=%+v", retained)
	}
	expired := telegramSettingsRequest(t, application, ownerSession, http.MethodPost, "/api/settings/telegram/confirm", map[string]any{"challenge_id": challenge.ID, "code": code})
	if expired.Code != http.StatusBadRequest {
		t.Fatalf("expired code status=%d body=%s", expired.Code, expired.Body.String())
	}
	application.telegramMu.Lock()
	_, remains := application.telegramBindings[challenge.ID]
	application.telegramMu.Unlock()
	if remains {
		t.Fatal("expired challenge was not removed")
	}
	if _, _, err := application.store.LoadTelegramConfig(t.Context()); err == nil {
		t.Fatal("invalid verification persisted Telegram configuration")
	}
}

func newTelegramSettingsTestApp(t *testing.T, client *http.Client) (*App, *session) {
	t.Helper()
	application, err := New(t.Context(), Config{
		DatabasePath: filepath.Join(t.TempDir(), "telegram-settings-test.db"), AdminPassword: "test-password",
		TelegramHTTPClient: client, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
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
	if err != nil || len(admins) != 1 {
		t.Fatalf("owner account=%+v err=%v", admins, err)
	}
	return application, application.sessions.create(admins[0].ID, admins[0].Username, true)
}
