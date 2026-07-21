package serverapp

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"testing"

	"smalux-speedtest/internal/store"
)

func TestAdminUserManagementAndSessionRevocation(t *testing.T) {
	application, err := New(t.Context(), Config{
		Listen: ":0", DatabasePath: filepath.Join(t.TempDir(), "admin-api.db"), AdminPassword: "initial-password",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer application.store.Close()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local sockets are unavailable: %v", err)
	}
	server := httptest.NewUnstartedServer(application.routes())
	server.Listener = listener
	server.Start()
	defer server.Close()

	adminClient, adminCSRF := loginAdminForTest(t, server.URL, "admin", "initial-password")
	created := doAdminJSON(t, adminClient, adminCSRF, http.MethodPost, server.URL+"/api/admin-users", map[string]any{
		"username": "operator", "password": "operator-password",
	})
	if created.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(created.Body)
		created.Body.Close()
		t.Fatalf("create administrator returned %d: %s", created.StatusCode, body)
	}
	var operator store.AdminUser
	if err := json.NewDecoder(created.Body).Decode(&operator); err != nil {
		created.Body.Close()
		t.Fatal(err)
	}
	created.Body.Close()
	if operator.Username != "operator" || operator.ID == "" {
		t.Fatalf("created administrator = %+v", operator)
	}

	operatorClient, _ := loginAdminForTest(t, server.URL, "operator", "operator-password")
	response, err := operatorClient.Get(server.URL + "/api/admin-users")
	if err != nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("operator administrator access = %v, %v", response, err)
	}
	response.Body.Close()

	disabled := doAdminJSON(t, adminClient, adminCSRF, http.MethodPatch, server.URL+"/api/admin-users/"+operator.ID, map[string]any{"enabled": false})
	if disabled.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(disabled.Body)
		disabled.Body.Close()
		t.Fatalf("disable administrator returned %d: %s", disabled.StatusCode, body)
	}
	disabled.Body.Close()
	response, err = operatorClient.Get(server.URL + "/api/admin-users")
	if err != nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("disabled administrator session = %v, %v", response, err)
	}
	response.Body.Close()

	users, err := application.store.ListAdminUsers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var currentID string
	for _, user := range users {
		if user.Username == "admin" {
			currentID = user.ID
		}
	}
	for _, test := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPatch, "/api/admin-users/" + currentID, map[string]any{"enabled": false}},
		{http.MethodDelete, "/api/admin-users/" + currentID, nil},
	} {
		response := doAdminJSON(t, adminClient, adminCSRF, test.method, server.URL+test.path, test.body)
		if response.StatusCode != http.StatusConflict {
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			t.Fatalf("self protection %s returned %d: %s", test.method, response.StatusCode, body)
		}
		response.Body.Close()
	}
}

func loginAdminForTest(t *testing.T, baseURL, username, password string) (*http.Client, string) {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	response, err := client.PostForm(baseURL+"/login", url.Values{"username": {username}, "password": {password}})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("login %q returned %d: %s", username, response.StatusCode, body)
	}
	match := regexp.MustCompile(`name="csrf-token" content="([^"]+)"`).FindSubmatch(body)
	if len(match) != 2 {
		t.Fatalf("CSRF token missing after login %q: %s", username, body)
	}
	return client, string(match[1])
}

func doAdminJSON(t *testing.T, client *http.Client, csrf, method, endpoint string, value any) *http.Response {
	t.Helper()
	var body io.Reader
	if value != nil {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, endpoint, body)
	if err != nil {
		t.Fatal(err)
	}
	if value != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("X-CSRF-Token", csrf)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}
