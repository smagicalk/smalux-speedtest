package serverapp

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestChangeOwnPasswordRotatesAdministratorSessions(t *testing.T) {
	application, err := New(t.Context(), Config{
		DatabasePath: filepath.Join(t.TempDir(), "account-password.db"), AdminPassword: "initial-password",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer application.store.Close()
	admin, err := application.store.AuthenticateAdmin(t.Context(), "admin", "initial-password")
	if err != nil {
		t.Fatal(err)
	}
	current := application.sessions.create(admin.ID, admin.Username, admin.IsOwner)
	otherDevice := application.sessions.create(admin.ID, admin.Username, admin.IsOwner)

	pageRequest := httptest.NewRequest(http.MethodGet, "/account", nil)
	pageRequest.AddCookie(&http.Cookie{Name: "smalux_session", Value: current.token})
	pageResponse := httptest.NewRecorder()
	application.routes().ServeHTTP(pageResponse, pageRequest)
	if pageResponse.Code != http.StatusOK || !strings.Contains(pageResponse.Body.String(), "账户安全") {
		t.Fatalf("account page status=%d body=%s", pageResponse.Code, pageResponse.Body.String())
	}

	payload, _ := json.Marshal(map[string]string{
		"current_password": "initial-password", "new_password": "replacement-password", "new_password_confirm": "replacement-password",
	})
	request := httptest.NewRequest(http.MethodPost, "/api/account/password", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", current.csrf)
	request.AddCookie(&http.Cookie{Name: "smalux_session", Value: current.token})
	response := httptest.NewRecorder()
	application.routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("password change status=%d body=%s", response.Code, response.Body.String())
	}
	if _, ok := application.sessions.get(current.token); ok {
		t.Fatal("current session was not rotated")
	}
	if _, ok := application.sessions.get(otherDevice.token); ok {
		t.Fatal("other device session remained active")
	}

	var replacementCookie *http.Cookie
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == "smalux_session" {
			replacementCookie = cookie
			break
		}
	}
	if replacementCookie == nil || replacementCookie.Value == current.token {
		t.Fatal("password change did not issue a replacement session cookie")
	}
	if replacement, ok := application.sessions.get(replacementCookie.Value); !ok || replacement.userID != admin.ID {
		t.Fatal("replacement session is not active for the administrator")
	}
	if _, err := application.store.AuthenticateAdmin(t.Context(), "admin", "initial-password"); err == nil {
		t.Fatal("old password remained valid")
	}
	if _, err := application.store.AuthenticateAdmin(t.Context(), "admin", "replacement-password"); err != nil {
		t.Fatalf("replacement password authentication failed: %v", err)
	}
}

func TestChangeOwnPasswordRejectsInvalidRequests(t *testing.T) {
	application, err := New(t.Context(), Config{
		DatabasePath: filepath.Join(t.TempDir(), "account-errors.db"), AdminPassword: "initial-password",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer application.store.Close()
	admin, _ := application.store.AuthenticateAdmin(t.Context(), "admin", "initial-password")

	tests := []struct {
		name       string
		payload    map[string]string
		wantStatus int
	}{
		{name: "wrong current password", payload: map[string]string{"current_password": "wrong-password", "new_password": "replacement-password", "new_password_confirm": "replacement-password"}, wantStatus: http.StatusBadRequest},
		{name: "mismatched confirmation", payload: map[string]string{"current_password": "initial-password", "new_password": "replacement-password", "new_password_confirm": "different-password"}, wantStatus: http.StatusBadRequest},
		{name: "invalid new password", payload: map[string]string{"current_password": "initial-password", "new_password": "short", "new_password_confirm": "short"}, wantStatus: http.StatusBadRequest},
		{name: "unchanged password", payload: map[string]string{"current_password": "initial-password", "new_password": "initial-password", "new_password_confirm": "initial-password"}, wantStatus: http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session := application.sessions.create(admin.ID, admin.Username, admin.IsOwner)
			body, _ := json.Marshal(test.payload)
			request := httptest.NewRequest(http.MethodPost, "/api/account/password", bytes.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-CSRF-Token", session.csrf)
			request.AddCookie(&http.Cookie{Name: "smalux_session", Value: session.token})
			response := httptest.NewRecorder()
			application.routes().ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status=%d want=%d body=%s", response.Code, test.wantStatus, response.Body.String())
			}
		})
	}
}
