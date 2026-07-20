package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"smalux-speedtest/internal/model"
)

// TestOpenPhysicallyPurgesLegacySensitiveBytes complements query-level migration
// tests by inspecting the database and sidecar files after close. It guards against
// old values remaining in free SQLite pages or obsolete WAL frames.
func TestOpenPhysicallyPurgesLegacySensitiveBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-physical.db")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	client, _, err := first.CreateClient(t.Context(), "client", nil)
	if err != nil {
		t.Fatal(err)
	}
	task := Task{ID: model.NewID(), Status: "completed", CandidateCount: 1, TopN: 1, Threads: 1, ProxyCount: 1, ClientCount: 1, CreatedAt: now()}
	if err := first.CreateTask(t.Context(), task, []string{client.ID}); err != nil {
		t.Fatal(err)
	}
	if err := first.SaveResult(t.Context(), model.SpeedResult{
		TaskID: task.ID, ClientID: client.ID, ProxyID: "proxy", ProxyName: "node", Protocol: "vless", CreatedAt: now(),
	}); err != nil {
		t.Fatal(err)
	}
	secret := []byte("vless://physical-sentinel:password@192.0.2.44:443/private/key")
	if _, err := first.db.ExecContext(t.Context(), `UPDATE results SET proxy_name=?,speed_server_name=?,error=?,created_at=?`, string(secret), string(secret), string(secret), string(secret)); err != nil {
		t.Fatal(err)
	}
	if _, err := first.db.ExecContext(t.Context(), `UPDATE tasks SET created_at=?,started_at=?,finished_at=? WHERE id=?`,
		string(secret), string(secret), string(secret), task.ID); err != nil {
		t.Fatal(err)
	}
	legacyLabels, _ := json.Marshal(map[string]string{"region": "cn-east", "outbound": string(secret), "endpoint": "192.0.2.44:443"})
	if _, err := first.db.ExecContext(t.Context(), `UPDATE clients SET name=?,labels_json=?,version=?,os=?,arch=? WHERE id=?`,
		string(secret), string(legacyLabels), string(secret), string(secret), string(secret), client.ID); err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{privacySensitiveScrubSetting, privacyProxyNameScrubSetting, privacyClientScrubSetting, privacyTimestampScrubSetting, privacyStoragePurgeSetting} {
		// A present but corrupted marker must not suppress a future scrub. Using a
		// non-complete value exercises that distinction while retaining the row so
		// the migration must use an upsert when it records completion.
		if _, err := first.db.ExecContext(t.Context(), `UPDATE settings SET value=? WHERE key=?`, "tampered", marker); err != nil {
			t.Fatal(err)
		}
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	clients, err := second.ListClients(t.Context())
	if err != nil || len(clients) != 1 {
		t.Fatalf("clients after privacy migration = %+v, %v", clients, err)
	}
	if clients[0].Name != "client-node" || !reflect.DeepEqual(clients[0].Labels, map[string]string{"region": "cn-east"}) ||
		clients[0].Version != "" || clients[0].OS != "" || clients[0].Arch != "" {
		t.Fatalf("legacy Client metadata was not scrubbed: %+v", clients[0])
	}
	for _, marker := range []string{privacyClientScrubSetting, privacyTimestampScrubSetting, privacyStoragePurgeSetting} {
		var value string
		if err := second.db.QueryRowContext(t.Context(), `SELECT value FROM settings WHERE key=?`, marker).Scan(&value); err != nil || value != "complete" {
			t.Fatalf("privacy marker %q = %q, %v", marker, value, err)
		}
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{path, path + "-wal"} {
		contents, err := os.ReadFile(candidate)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(contents, secret) {
			t.Fatalf("legacy secret remains in %s", filepath.Base(candidate))
		}
	}
}
