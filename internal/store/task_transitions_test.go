package store

import (
	"path/filepath"
	"testing"

	"smalux-speedtest/internal/model"
)

// TestFinishTargetAggregatesInOneTransaction 覆盖 ACK、单目标终结和最终 partial 聚合，
// 并验证任务终态后迟到转换不能重新打开数据库状态。
func TestFinishTargetAggregatesInOneTransaction(t *testing.T) {
	database, clients := newTransitionStore(t, 2)
	task := Task{ID: model.NewID(), Status: "queued", CandidateCount: 10, TopN: 3, Threads: 4, ProxyCount: 1, CreatedAt: now()}
	if err := database.CreateTask(t.Context(), task, []string{clients[0].ID, clients[1].ID}); err != nil {
		t.Fatal(err)
	}
	started, err := database.StartTarget(t.Context(), task.ID, clients[0].ID)
	if err != nil || !started {
		t.Fatalf("StartTarget = %v, %v", started, err)
	}
	if started, err := database.StartTarget(t.Context(), task.ID, clients[0].ID); err != nil || started {
		t.Fatalf("duplicate StartTarget = %v, %v", started, err)
	}
	first, err := database.FinishTarget(t.Context(), task.ID, clients[0].ID, "completed", "")
	if err != nil || first.TaskTerminal || first.TargetStatus != "completed" {
		t.Fatalf("first transition = %+v, %v", first, err)
	}
	second, err := database.FinishTarget(t.Context(), task.ID, clients[1].ID, "failed", "client failed")
	if err != nil || !second.TaskTerminal || second.TaskStatus != "partial" {
		t.Fatalf("second transition = %+v, %v", second, err)
	}
	stored, err := database.GetTask(t.Context(), task.ID)
	if err != nil || stored.Status != "partial" || stored.FinishedAt == "" {
		t.Fatalf("stored task = %+v, %v", stored, err)
	}
	if started, err := database.StartTarget(t.Context(), task.ID, clients[1].ID); err != nil || started {
		t.Fatalf("late StartTarget = %v, %v", started, err)
	}
}

// TestCancelTaskPersistsTargets 验证取消事务保留已完成目标、终结所有活动目标，并使断线
// 重排队条件更新失效，避免 task_targets 与父任务状态分叉。
func TestCancelTaskPersistsTargets(t *testing.T) {
	database, clients := newTransitionStore(t, 2)
	task := Task{ID: model.NewID(), Status: "queued", CandidateCount: 10, TopN: 3, Threads: 4, ProxyCount: 1, CreatedAt: now()}
	if err := database.CreateTask(t.Context(), task, []string{clients[0].ID, clients[1].ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.FinishTarget(t.Context(), task.ID, clients[0].ID, "completed", ""); err != nil {
		t.Fatal(err)
	}
	if started, err := database.StartTarget(t.Context(), task.ID, clients[1].ID); err != nil || !started {
		t.Fatalf("StartTarget = %v, %v", started, err)
	}
	if err := database.CancelTask(t.Context(), task.ID, "canceled by test"); err != nil {
		t.Fatal(err)
	}
	if err := database.CancelTask(t.Context(), task.ID, "canceled again"); err != nil {
		t.Fatalf("idempotent cancellation failed: %v", err)
	}
	stored, err := database.GetTask(t.Context(), task.ID)
	if err != nil || stored.Status != "canceled" || stored.FinishedAt == "" {
		t.Fatalf("stored canceled task = %+v, %v", stored, err)
	}
	completed, failed, canceled, total, err := database.TargetSummary(t.Context(), task.ID)
	if err != nil || completed != 1 || failed != 0 || canceled != 1 || total != 2 {
		t.Fatalf("target summary = completed:%d failed:%d canceled:%d total:%d error:%v", completed, failed, canceled, total, err)
	}
	if changed, err := database.RequeueTarget(t.Context(), task.ID, clients[1].ID, "late disconnect"); err != nil || changed {
		t.Fatalf("late RequeueTarget = %v, %v", changed, err)
	}
}

func newTransitionStore(t *testing.T, clientCount int) (*Store, []Client) {
	t.Helper()
	database, err := Open(filepath.Join(t.TempDir(), "transitions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	clients := make([]Client, 0, clientCount)
	for index := 0; index < clientCount; index++ {
		client, _, err := database.CreateClient(t.Context(), "client-"+string(rune('a'+index)), nil)
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, client)
	}
	return database, clients
}
