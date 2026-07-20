package store

import (
	"path/filepath"
	"testing"

	"smalux-speedtest/internal/model"
)

// TestOpenScrubsLegacySensitiveData 模拟升级前数据库中已经存在的地址、任意 Client
// 元数据和错误原文。节点显示名称应保留，其余字段必须被 v2 迁移覆盖。
func TestOpenScrubsLegacySensitiveData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-sensitive-errors.db")
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
	result := model.SpeedResult{
		TaskID: task.ID, ClientID: client.ID, ProxyID: "proxy", ProxyName: "node", Protocol: "vless",
		MaskedAddress: "*.example.com:443", SpeedServerID: "server", CreatedAt: now(),
	}
	if err := first.SaveResult(t.Context(), result); err != nil {
		t.Fatal(err)
	}
	const secret = "vless://uuid:password@secret.example:443"
	if _, err := first.db.ExecContext(t.Context(), `UPDATE results SET
		masked_address=?,speed_server_id=?,speed_server_name=?,speed_server_host=?,country=?,sponsor=?`,
		"192.0.2.10:443", secret, secret, "192.0.2.20/private/path", secret, secret); err != nil {
		first.Close()
		t.Fatal(err)
	}
	for _, statement := range []string{
		`UPDATE results SET error=?`,
		`UPDATE task_targets SET error=?`,
		`UPDATE tasks SET error=?`,
	} {
		if _, err := first.db.ExecContext(t.Context(), statement, secret); err != nil {
			first.Close()
			t.Fatal(err)
		}
	}
	if _, err := first.db.ExecContext(t.Context(), `DELETE FROM settings WHERE key=?`, privacySensitiveScrubSetting); err != nil {
		first.Close()
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	results, err := second.ListResults(t.Context(), task.ID)
	if err != nil || len(results) != 1 || results[0].Error != model.ResultErrorProxyTest {
		t.Fatalf("results after scrub = %+v, %v", results, err)
	}
	storedResult := results[0]
	if storedResult.ProxyName != "node" || storedResult.MaskedAddress != "[redacted]" || storedResult.SpeedServerName != legacySpeedServerName || storedResult.SpeedServerHost != "" || storedResult.Country != "" || storedResult.Sponsor != "" {
		t.Fatalf("legacy metadata after scrub = %+v", storedResult)
	}
	if len(storedResult.SpeedServerID) <= len("server-") || storedResult.SpeedServerID == secret {
		t.Fatalf("legacy server ID was not anonymized: %q", storedResult.SpeedServerID)
	}
	stored, err := second.GetTask(t.Context(), task.ID)
	if err != nil || stored.Error != model.TaskFailureClient {
		t.Fatalf("task after scrub = %+v, %v", stored, err)
	}
	var targetError string
	if err := second.db.QueryRowContext(t.Context(), `SELECT error FROM task_targets WHERE task_id=? AND client_id=?`, task.ID, client.ID).Scan(&targetError); err != nil {
		t.Fatal(err)
	}
	if targetError != model.TaskFailureClient {
		t.Fatalf("target error after scrub = %q", targetError)
	}
}

func TestStoreNormalizesProxyNamesAtPersistenceBoundary(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "proxy-name-boundary.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	client, _, err := database.CreateClient(t.Context(), "client", nil)
	if err != nil {
		t.Fatal(err)
	}
	task := Task{ID: model.NewID(), Status: "completed", CandidateCount: 1, TopN: 1, Threads: 1, ProxyCount: 2, ClientCount: 1, CreatedAt: now()}
	if err := database.CreateTask(t.Context(), task, []string{client.ID}); err != nil {
		t.Fatal(err)
	}
	for _, result := range []model.SpeedResult{
		{TaskID: task.ID, ClientID: client.ID, ProxyID: model.NewID(), ProxyName: "Tokyo 01", Protocol: "vless", CreatedAt: now()},
		{TaskID: task.ID, ClientID: client.ID, ProxyID: model.NewID(), ProxyName: "vless://uuid:password@private.example:443", Protocol: "vless", CreatedAt: now()},
	} {
		if err := database.SaveResult(t.Context(), result); err != nil {
			t.Fatal(err)
		}
	}
	results, err := database.ListResults(t.Context(), task.ID)
	if err != nil || len(results) != 2 {
		t.Fatalf("ListResults = %+v, %v", results, err)
	}
	names := make(map[string]string, len(results))
	for _, result := range results {
		names[result.ProxyID] = result.ProxyName
	}
	if len(names) != 2 {
		t.Fatalf("unexpected persisted proxy IDs: %+v", names)
	}
	var normal, sensitive int
	for _, name := range names {
		switch name {
		case "Tokyo 01":
			normal++
		case "vless-node":
			sensitive++
		}
	}
	if normal != 1 || sensitive != 1 {
		t.Fatalf("unexpected persisted proxy names: %+v", names)
	}
}

func TestOpenScrubsLegacySensitiveProxyNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-proxy-name.db")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	client, _, err := first.CreateClient(t.Context(), "client", nil)
	if err != nil {
		first.Close()
		t.Fatal(err)
	}
	task := Task{ID: model.NewID(), Status: "completed", CandidateCount: 1, TopN: 1, Threads: 1, ProxyCount: 2, ClientCount: 1, CreatedAt: now()}
	if err := first.CreateTask(t.Context(), task, []string{client.ID}); err != nil {
		first.Close()
		t.Fatal(err)
	}
	proxyIDs := []string{model.NewID(), model.NewID()}
	for _, proxyID := range proxyIDs {
		if err := first.SaveResult(t.Context(), model.SpeedResult{
			TaskID: task.ID, ClientID: client.ID, ProxyID: proxyID, ProxyName: "placeholder", Protocol: "vless", CreatedAt: now(),
		}); err != nil {
			first.Close()
			t.Fatal(err)
		}
	}
	const legacySecret = "vless://uuid:password@legacy.example:443/private/path"
	if _, err := first.db.ExecContext(t.Context(), `UPDATE results SET proxy_name=CASE proxy_id WHEN ? THEN 'Tokyo 01' ELSE ? END`, normalizePersistedProxyID(proxyIDs[0]), legacySecret); err != nil {
		first.Close()
		t.Fatal(err)
	}
	// Open already wrote both markers. Retain the v2 marker and remove only v3 to
	// model a database upgraded by the previous privacy release.
	if _, err := first.db.ExecContext(t.Context(), `DELETE FROM settings WHERE key=?`, privacyProxyNameScrubSetting); err != nil {
		first.Close()
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	rows, err := second.db.QueryContext(t.Context(), `SELECT proxy_id,proxy_name FROM results ORDER BY proxy_id`)
	if err != nil {
		t.Fatal(err)
	}
	names := make(map[string]string)
	for rows.Next() {
		var proxyID, name string
		if err := rows.Scan(&proxyID, &name); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		names[proxyID] = name
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	var normal, sensitive int
	for _, name := range names {
		switch name {
		case "Tokyo 01":
			normal++
		case "vless-node":
			sensitive++
		}
	}
	if normal != 1 || sensitive != 1 {
		t.Fatalf("legacy proxy names after v3 scrub: %+v", names)
	}
	var marker string
	if err := second.db.QueryRowContext(t.Context(), `SELECT value FROM settings WHERE key=?`, privacyProxyNameScrubSetting).Scan(&marker); err != nil || marker != "complete" {
		t.Fatalf("v3 marker = %q, %v", marker, err)
	}
}
