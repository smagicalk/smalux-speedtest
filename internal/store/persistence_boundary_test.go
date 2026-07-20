package store

import (
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"

	"smalux-speedtest/internal/model"
)

// TestSaveResultSanitizesDirectStoreInput deliberately bypasses Hub validation.
// The database row must still contain only bounded public metadata and server-owned
// identity/time values.
func TestSaveResultSanitizesDirectStoreInput(t *testing.T) {
	database, client, task := newBoundaryTask(t)
	sentinel := "STORE_BOUNDARY_SENTINEL_vless://uuid:password@192.0.2.44:443/private"
	if err := database.SaveResult(t.Context(), model.SpeedResult{
		TaskID: task.ID, ClientID: client.ID, ProxyID: sentinel, ProxyName: sentinel,
		Protocol: sentinel, MaskedAddress: sentinel, SpeedServerID: sentinel,
		SpeedServerName: "speed.private.example", SpeedServerHost: sentinel,
		Country: "country.private.example", Sponsor: "sponsor.private.example",
		Error: sentinel, CreatedAt: sentinel,
	}); err != nil {
		t.Fatal(err)
	}
	if err := database.SaveResult(t.Context(), model.SpeedResult{
		TaskID: task.ID, ClientID: client.ID, ProxyID: model.NewID(), ProxyName: "Tokyo 01",
		Protocol: "VLESS", MaskedAddress: "[redacted]:443", SpeedServerID: model.NewID(),
		SpeedServerName: "Tokyo", Country: "Japan", Sponsor: "Example ISP", CreatedAt: sentinel,
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := database.db.QueryContext(t.Context(), `SELECT proxy_id,protocol,proxy_name,masked_address,speed_server_id,speed_server_name,speed_server_host,country,sponsor,error,created_at FROM results ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var foundSanitized, foundNormal bool
	for rows.Next() {
		var proxyID, protocol, proxyName, maskedAddress, serverID, serverName, serverHost, country, sponsor, resultError, createdAt string
		if err := rows.Scan(&proxyID, &protocol, &proxyName, &maskedAddress, &serverID, &serverName, &serverHost, &country, &sponsor, &resultError, &createdAt); err != nil {
			t.Fatal(err)
		}
		persisted := strings.Join([]string{proxyID, protocol, proxyName, maskedAddress, serverID, serverName, serverHost, country, sponsor, resultError, createdAt}, "\x00")
		if strings.Contains(persisted, sentinel) {
			t.Fatalf("client sentinel persisted: %q", persisted)
		}
		if len(proxyID) != 32 {
			t.Fatalf("proxy_id is not a 32-hex digest: %q", proxyID)
		}
		if _, err := hex.DecodeString(proxyID); err != nil {
			t.Fatalf("proxy_id is not hex: %q", proxyID)
		}
		if protocol == model.ResultProtocolUnknown {
			if serverName != "" || country != "" || sponsor != "" {
				t.Fatalf("bare-domain metadata survived: name=%q country=%q sponsor=%q", serverName, country, sponsor)
			}
			if maskedAddress != "[redacted]" || serverHost != "" || resultError != model.ResultErrorProxyTest {
				t.Fatalf("unsafe result fields survived: address=%q host=%q error=%q", maskedAddress, serverHost, resultError)
			}
			foundSanitized = true
		}
		if protocol == "vless" {
			if serverName != "Tokyo" || country != "Japan" || sponsor != "Example ISP" {
				t.Fatalf("normal public metadata was lost: %q %q %q", serverName, country, sponsor)
			}
			foundNormal = true
		}
		if createdAt == sentinel {
			t.Fatal("client-controlled created_at persisted")
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if foundSanitized && foundNormal {
		return
	}
	t.Fatalf("boundary rows missing sanitized=%v normal=%v", foundSanitized, foundNormal)
}

func TestListResultsSanitizesLegacyReadFields(t *testing.T) {
	database, client, task := newBoundaryTask(t)
	if err := database.SaveResult(t.Context(), model.SpeedResult{
		TaskID: task.ID, ClientID: client.ID, ProxyID: model.NewID(), ProxyName: "Tokyo 01", Protocol: "vless",
		MaskedAddress: "[redacted]:443", SpeedServerID: "server-0123456789abcdef", SpeedServerName: "Tokyo",
	}); err != nil {
		t.Fatal(err)
	}
	const legacy = "vless://legacy:password@secret.example:443"
	if _, err := database.db.ExecContext(t.Context(), `UPDATE results SET masked_address=?,speed_server_id=?,speed_server_host=?,proxy_name=?,speed_server_name=?`,
		"203.0.113.8:443", legacy, legacy, legacy, legacy); err != nil {
		t.Fatal(err)
	}
	results, err := database.ListResults(t.Context(), task.ID)
	if err != nil || len(results) != 1 {
		t.Fatalf("ListResults=%+v, %v", results, err)
	}
	result := results[0]
	if result.MaskedAddress != "[redacted]" || result.SpeedServerHost != "" || result.SpeedServerName != "" ||
		result.ProxyName != "vless-node" || result.SpeedServerID == legacy || result.SpeedServerID == "" {
		t.Fatalf("legacy read fields were not normalized: %+v", result)
	}
}

func TestTaskErrorWritersNormalizeDirectStoreInput(t *testing.T) {
	sentinel := "STORE_TASK_SENTINEL_vless://uuid:password@192.0.2.44:443"
	tests := []struct {
		name string
		call func(*Store, string, string) error
		want string
	}{
		{name: "task status", call: func(s *Store, taskID, clientID string) error {
			return s.SetTaskStatus(t.Context(), taskID, "failed", sentinel)
		}},
		{name: "target status", call: func(s *Store, taskID, clientID string) error {
			return s.SetTargetStatus(t.Context(), taskID, clientID, "failed", sentinel)
		}},
		{name: "cancel task", call: func(s *Store, taskID, clientID string) error {
			return s.CancelTask(t.Context(), taskID, sentinel)
		}},
		{name: "fail task", call: func(s *Store, taskID, clientID string) error {
			return s.FailTask(t.Context(), taskID, sentinel)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			database, client, task := newBoundaryTask(t)
			if test.name == "target status" {
				// SetTargetStatus is intentionally direct and does not require a running target.
			}
			if err := test.call(database, task.ID, client.ID); err != nil {
				t.Fatal(err)
			}
			var taskError, targetError string
			if err := database.db.QueryRowContext(t.Context(), `SELECT error FROM tasks WHERE id=?`, task.ID).Scan(&taskError); err != nil {
				t.Fatal(err)
			}
			if err := database.db.QueryRowContext(t.Context(), `SELECT error FROM task_targets WHERE task_id=? AND client_id=?`, task.ID, client.ID).Scan(&targetError); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(taskError+targetError, sentinel) {
				t.Fatalf("task sentinel persisted: task=%q target=%q", taskError, targetError)
			}
			if test.name == "target status" && targetError != model.TaskFailureClient {
				t.Fatalf("target error=%q, want normalized client failure", targetError)
			}
		})
	}
}

func TestTaskTransitionDetailsKeepServerCategories(t *testing.T) {
	database, client, task := newBoundaryTask(t)
	if err := database.CancelTask(t.Context(), task.ID, taskDetailCanceledByAdministrator); err != nil {
		t.Fatal(err)
	}
	var detail string
	if err := database.db.QueryRowContext(t.Context(), `SELECT error FROM tasks WHERE id=?`, task.ID).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if detail != taskDetailCanceledByAdministrator {
		t.Fatalf("server detail=%q, want %q", detail, taskDetailCanceledByAdministrator)
	}
	_ = client
}

func TestTargetTransitionWritersNormalizeDirectInput(t *testing.T) {
	sentinel := "STORE_TRANSITION_SENTINEL_vless://uuid:password@192.0.2.44:443"
	database, client, task := newBoundaryTask(t)
	started, err := database.StartTarget(t.Context(), task.ID, client.ID)
	if err != nil || !started {
		t.Fatalf("StartTarget=%v, %v", started, err)
	}
	changed, err := database.RequeueTarget(t.Context(), task.ID, client.ID, sentinel)
	if err != nil || !changed {
		t.Fatalf("RequeueTarget=%v, %v", changed, err)
	}
	var detail string
	if err := database.db.QueryRowContext(t.Context(), `SELECT error FROM task_targets WHERE task_id=? AND client_id=?`, task.ID, client.ID).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if detail != model.TaskFailureClient || strings.Contains(detail, sentinel) {
		t.Fatalf("requeue detail=%q", detail)
	}

	database2, client2, task2 := newBoundaryTask(t)
	if _, err := database2.FinishTarget(t.Context(), task2.ID, client2.ID, "failed", sentinel); err != nil {
		t.Fatal(err)
	}
	if err := database2.db.QueryRowContext(t.Context(), `SELECT error FROM task_targets WHERE task_id=? AND client_id=?`, task2.ID, client2.ID).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if detail != model.TaskFailureClient || strings.Contains(detail, sentinel) {
		t.Fatalf("finish detail=%q", detail)
	}
}

func newBoundaryTask(t *testing.T) (*Store, Client, Task) {
	t.Helper()
	database, err := Open(filepath.Join(t.TempDir(), "boundary.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	client, _, err := database.CreateClient(t.Context(), "boundary-client", nil)
	if err != nil {
		t.Fatal(err)
	}
	task := Task{ID: model.NewID(), Status: "queued", CandidateCount: 1, TopN: 1, Threads: 1, ProxyCount: 1, CreatedAt: now()}
	if err := database.CreateTask(t.Context(), task, []string{client.ID}); err != nil {
		t.Fatal(err)
	}
	return database, client, task
}
