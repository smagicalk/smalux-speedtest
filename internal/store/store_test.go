package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"smalux-speedtest/internal/model"
)

func TestStoreClientTaskAndResult(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.BootstrapAdmin(ctx, "test-password"); err != nil {
		t.Fatal(err)
	}
	if !database.VerifyAdmin(ctx, "test-password") || database.VerifyAdmin(ctx, "wrong-password") {
		t.Fatal("admin password verification failed")
	}
	client, token, err := database.CreateClient(ctx, "client-1", map[string]string{"region": "test"})
	if err != nil {
		t.Fatal(err)
	}
	authenticated, err := database.AuthenticateClient(ctx, token)
	if err != nil || authenticated.ID != client.ID {
		t.Fatalf("client authentication failed: %+v %v", authenticated, err)
	}
	task := Task{ID: model.NewID(), Status: "queued", CandidateCount: 10, TopN: 3, ProxyCount: 1, ClientCount: 1, CreatedAt: now()}
	if err := database.CreateTask(ctx, task, []string{client.ID}); err != nil {
		t.Fatal(err)
	}
	result := model.SpeedResult{
		TaskID: task.ID, ClientID: client.ID, ProxyID: "proxy-1", ProxyName: "node", Protocol: "vless",
		MaskedAddress: "*.example.com:443", SpeedServerID: "123", LatencyMS: 10, DownloadBPS: 100_000_000,
	}
	if err := database.SaveResult(ctx, result); err != nil {
		t.Fatal(err)
	}
	results, err := database.ListResults(ctx, task.ID)
	if err != nil || len(results) != 1 || results[0].DownloadBPS != result.DownloadBPS || results[0].ClientName != client.Name {
		t.Fatalf("unexpected results: %+v %v", results, err)
	}
	storedTask, err := database.GetTask(ctx, task.ID)
	if err != nil || storedTask.Threads != 4 {
		t.Fatalf("unexpected default thread count: %+v %v", storedTask, err)
	}
	if err := database.RevokeClient(ctx, client.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.AuthenticateClient(ctx, token); err == nil {
		t.Fatal("revoked token was accepted")
	}
}

func TestOpenMigratesTaskThreads(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`CREATE TABLE tasks (
		id TEXT PRIMARY KEY, status TEXT NOT NULL, candidate_count INTEGER NOT NULL, top_n INTEGER NOT NULL,
		proxy_count INTEGER NOT NULL, client_count INTEGER NOT NULL, error TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL, started_at TEXT NOT NULL DEFAULT '', finished_at TEXT NOT NULL DEFAULT ''
	)`)
	if err != nil {
		t.Fatal(err)
	}
	createdAt := now()
	if _, err := legacy.Exec(`INSERT INTO tasks(id,status,candidate_count,top_n,proxy_count,client_count,created_at) VALUES('legacy','completed',10,3,1,1,?)`, createdAt); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	task, err := database.GetTask(ctx, "legacy")
	if err != nil || task.Threads != 4 {
		t.Fatalf("legacy task was not migrated: %+v %v", task, err)
	}
}

func TestOpenMarksInterruptedTasksFailed(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "restart.db")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.BootstrapAdmin(ctx, "test-password"); err != nil {
		t.Fatal(err)
	}
	client, _, err := first.CreateClient(ctx, "client", nil)
	if err != nil {
		t.Fatal(err)
	}
	task := Task{ID: model.NewID(), Status: "running", CandidateCount: 10, TopN: 1, ProxyCount: 1, CreatedAt: now()}
	if err := first.CreateTask(ctx, task, []string{client.ID}); err != nil {
		t.Fatal(err)
	}
	first.Close()
	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	stored, err := second.GetTask(ctx, task.ID)
	if err != nil || stored.Status != "failed" {
		t.Fatalf("unexpected task after restart: %+v %v", stored, err)
	}
}
