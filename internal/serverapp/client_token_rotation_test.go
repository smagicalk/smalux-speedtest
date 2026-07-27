package serverapp

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"smalux-speedtest/internal/store"
)

func TestClientTokenRotationAPI(t *testing.T) {
	application, err := New(t.Context(), Config{Listen: ":0", DatabasePath: filepath.Join(t.TempDir(), "rotation.db"), AdminPassword: "initial-password"})
	if err != nil {
		t.Fatal(err)
	}
	defer application.store.Close()
	server := newTestHTTPServer(t, application)
	defer server.Close()

	adminClient, csrf := loginAdminForTest(t, server.URL, "admin", "initial-password")
	createdResponse := doAdminJSON(t, adminClient, csrf, http.MethodPost, server.URL+"/api/clients", map[string]any{"name": "migration-node"})
	var created struct {
		Client store.Client `json:"client"`
		Token  string       `json:"token"`
	}
	if err := json.NewDecoder(createdResponse.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	createdResponse.Body.Close()

	rotatedResponse := doAdminJSON(t, adminClient, csrf, http.MethodPost, server.URL+"/api/clients/"+created.Client.ID+"/token", nil)
	if rotatedResponse.StatusCode != http.StatusOK {
		t.Fatalf("rotate status = %d", rotatedResponse.StatusCode)
	}
	if rotatedResponse.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("rotation cache policy = %q", rotatedResponse.Header.Get("Cache-Control"))
	}
	var rotated struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(rotatedResponse.Body).Decode(&rotated); err != nil {
		t.Fatal(err)
	}
	rotatedResponse.Body.Close()
	if rotated.Token == "" || rotated.Token == created.Token {
		t.Fatal("rotation response did not contain a replacement token")
	}
	if _, err := application.store.AuthenticateClient(t.Context(), created.Token); err == nil {
		t.Fatal("old token still authenticates through API rotation")
	}
	authenticated, err := application.store.AuthenticateClient(t.Context(), rotated.Token)
	if err != nil || authenticated.ID != created.Client.ID {
		t.Fatalf("new token authentication = %+v, %v", authenticated, err)
	}
}
