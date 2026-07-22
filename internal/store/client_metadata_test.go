package store

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestCreateClientNormalizesPublicMetadata covers both the Store return value and the
// JSON shape embedded by the management API. Ordinary operational labels remain useful;
// endpoint-, path- and credential-shaped values never reach either representation.
func TestCreateClientNormalizesPublicMetadata(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "client-metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	labels := map[string]string{
		"region":       "cn-east",
		"provider":     "Example Telecom",
		"bad.key":      "must be removed",
		"endpoint":     "192.0.2.10:443",
		"private_path": "../etc/smalux/client.key",
		"credential":   "vless://uuid:password@private.example:443/private/path",
	}
	client, token, err := database.CreateClient(t.Context(), "aws-sg-01", labels)
	if err != nil {
		t.Fatal(err)
	}
	wantLabels := map[string]string{"provider": "Example Telecom", "region": "cn-east"}
	if client.Name != "aws-sg-01" || !reflect.DeepEqual(client.Labels, wantLabels) {
		t.Fatalf("CreateClient metadata = %+v, want name and labels %+v", client, wantLabels)
	}
	if strings.Contains(token, client.ID) {
		t.Fatal("new Client token unexpectedly contains its public Client ID")
	}
	authenticated, err := database.AuthenticateClient(t.Context(), token)
	if err != nil || authenticated.Name != client.Name || !reflect.DeepEqual(authenticated.Labels, wantLabels) {
		t.Fatalf("authenticated Client metadata = %+v, %v", authenticated, err)
	}
	publicJSON, err := json.Marshal(map[string]any{"client": client, "token": token})
	if err != nil {
		t.Fatal(err)
	}
	for _, sensitive := range []string{"192.0.2.10", "../etc/smalux", "vless://", "private.example", "password"} {
		if strings.Contains(string(publicJSON), sensitive) {
			t.Fatalf("management JSON contains %q: %s", sensitive, publicJSON)
		}
	}
	// The one-time token is intentionally present in the creation response; only managed
	// metadata is filtered. The token itself is stored solely as a hash.
	if !strings.Contains(string(publicJSON), token) {
		t.Fatal("creation JSON did not contain the one-time Client token")
	}
}

func TestCreateClientFallsBackForSensitiveName(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "client-name.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	client, _, err := database.CreateClient(t.Context(), "vless://uuid:password@192.0.2.10:443/private/path", nil)
	if err != nil {
		t.Fatal(err)
	}
	if client.Name != "client-node" {
		t.Fatalf("sensitive Client name persisted as %q", client.Name)
	}
}
