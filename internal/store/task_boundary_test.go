package store

import (
	"path/filepath"
	"strings"
	"testing"

	"smalux-speedtest/internal/model"
)

func TestCreateTaskOwnsIdentityAndTimestamp(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "task-boundary.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	client, _, err := database.CreateClient(t.Context(), "client", nil)
	if err != nil {
		t.Fatal(err)
	}
	const secret = "vless://task-secret@192.0.2.50/private/path"
	invalid := Task{ID: secret, Status: "queued", Threads: 1, CreatedAt: secret}
	if err := database.CreateTask(t.Context(), invalid, []string{client.ID}); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("unsafe task ID error = %v", err)
	}

	task := Task{ID: model.NewID(), Status: "queued", Threads: 1, CreatedAt: secret}
	if err := database.CreateTask(t.Context(), task, []string{client.ID}); err != nil {
		t.Fatal(err)
	}
	stored, err := database.GetTask(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.CreatedAt == secret || strings.Contains(stored.CreatedAt, "vless://") {
		t.Fatalf("caller-controlled task timestamp persisted: %q", stored.CreatedAt)
	}
	if err := database.SetTaskStatus(t.Context(), task.ID, secret, secret); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("unsafe task status error = %v", err)
	}
}
