package store

import (
	"path/filepath"
	"reflect"
	"testing"

	"smalux-speedtest/internal/model"
)

func TestUpdateClientHelloPreservesManagedIdentity(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "client-hello.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	managedLabels := map[string]string{"region": "managed", "provider": "example"}
	client, _, err := database.CreateClient(t.Context(), "managed-client", managedLabels)
	if err != nil {
		t.Fatal(err)
	}

	const secret = "vless://uuid:password@secret.example:443/private/key"
	if err := database.UpdateClientHello(t.Context(), client.ID, model.Hello{
		Name: secret, Labels: map[string]string{"outbound": secret},
		Version: secret, OS: secret, Arch: secret,
	}); err != nil {
		t.Fatal(err)
	}
	stored := onlyStoredClient(t, database)
	if stored.Name != client.Name || !reflect.DeepEqual(stored.Labels, managedLabels) {
		t.Fatalf("hello replaced managed identity: %+v", stored)
	}
	if stored.Version != "" || stored.OS != "" || stored.Arch != "" {
		t.Fatalf("unsafe runtime metadata was persisted: %+v", stored)
	}

	if err := database.UpdateClientHello(t.Context(), client.ID, model.Hello{
		Name: "ignored-name", Labels: map[string]string{"ignored": "label"},
		Version: "V1.2.3-RC_1", OS: " LINUX ", Arch: "AMD64",
	}); err != nil {
		t.Fatal(err)
	}
	stored = onlyStoredClient(t, database)
	if stored.Name != client.Name || !reflect.DeepEqual(stored.Labels, managedLabels) {
		t.Fatalf("safe hello replaced managed identity: %+v", stored)
	}
	if stored.Version != "V1.2.3-RC_1" || stored.OS != "linux" || stored.Arch != "amd64" {
		t.Fatalf("safe runtime metadata was not normalized: %+v", stored)
	}
}

func TestNormalizeClientVersionRejectsSensitiveShapes(t *testing.T) {
	tests := map[string]string{
		"dev":                              "dev",
		"v1.2.3-rc_1":                      "v1.2.3-rc_1",
		"2026.07.20":                       "2026.07.20",
		"vless://uuid@secret.example":      "",
		"192.0.2.10":                       "",
		"../private/key":                   "",
		"0123456789abcdef0123456789abcdef": "",
		"version with spaces":              "",
	}
	for input, expected := range tests {
		if actual := normalizeClientVersion(input); actual != expected {
			t.Fatalf("normalizeClientVersion(%q) = %q, want %q", input, actual, expected)
		}
	}
}

func onlyStoredClient(t *testing.T, database *Store) Client {
	t.Helper()
	clients, err := database.ListClients(t.Context())
	if err != nil || len(clients) != 1 {
		t.Fatalf("stored clients = %+v, %v", clients, err)
	}
	return clients[0]
}
