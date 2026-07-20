package store

import (
	"context"
	"database/sql"
	"errors"
)

// TargetTransition 是一次 Client 目标终结后数据库确认的权威状态。
// TaskTerminal 为 true 时，TaskStatus 和 Detail 可直接用于 Hub 清理与终态通知。
type TargetTransition struct {
	TargetStatus string
	TaskStatus   string
	Detail       string
	TaskTerminal bool
}

// StartTarget 原子地把 queued 目标及其父任务推进到 running。
// 父任务或目标已经离开活动状态时返回 false，避免迟到 ACK 覆盖 canceled 等终态。
func (s *Store) StartTarget(ctx context.Context, taskID, clientID string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE task_targets SET status='running',error=''
		WHERE task_id=? AND client_id=? AND status='queued'
		AND EXISTS(SELECT 1 FROM tasks WHERE id=? AND status IN ('queued','running'))`, taskID, clientID, taskID)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if changed == 0 {
		return false, tx.Commit()
	}
	startedAt := now()
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET status='running',error='',
		started_at=CASE WHEN started_at='' THEN ? ELSE started_at END
		WHERE id=? AND status IN ('queued','running')`, startedAt, taskID); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// RequeueTarget 把活动任务中的 running 目标退回 queued。
// 条件更新阻止连接关闭清理在任务取消或完成后重新打开目标状态。
func (s *Store) RequeueTarget(ctx context.Context, taskID, clientID, detail string) (bool, error) {
	detail = normalizePersistedTaskDetail(detail)
	result, err := s.db.ExecContext(ctx, `UPDATE task_targets SET status='queued',error=?
		WHERE task_id=? AND client_id=? AND status='running'
		AND EXISTS(SELECT 1 FROM tasks WHERE id=? AND status IN ('queued','running'))`, detail, taskID, clientID, taskID)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed > 0, err
}

// FinishTarget 在一个事务中终结目标、聚合全部目标，并在满足条件时终结父任务。
// 事务成功后调用方才可修改 Hub 内存状态，从而避免 SQLite 与 runtimeTask 永久分叉。
func (s *Store) FinishTarget(ctx context.Context, taskID, clientID, status, detail string) (TargetTransition, error) {
	if !isTargetTerminal(status) {
		return TargetTransition{}, errors.New("invalid terminal target status")
	}
	detail = normalizePersistedTaskDetail(detail)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TargetTransition{}, err
	}
	defer tx.Rollback()

	var currentTarget, currentTask, currentDetail string
	err = tx.QueryRowContext(ctx, `SELECT tt.status,t.status,t.error FROM task_targets tt
		JOIN tasks t ON t.id=tt.task_id WHERE tt.task_id=? AND tt.client_id=?`, taskID, clientID).
		Scan(&currentTarget, &currentTask, &currentDetail)
	if err != nil {
		return TargetTransition{}, err
	}
	currentDetail = normalizePersistedTaskDetail(currentDetail)
	if isTaskTerminal(currentTask) {
		if err := tx.Commit(); err != nil {
			return TargetTransition{}, err
		}
		return TargetTransition{TargetStatus: currentTarget, TaskStatus: currentTask, Detail: currentDetail, TaskTerminal: true}, nil
	}
	if !isTargetTerminal(currentTarget) {
		if _, err := tx.ExecContext(ctx, `UPDATE task_targets SET status=?,error=? WHERE task_id=? AND client_id=?`, status, detail, taskID, clientID); err != nil {
			return TargetTransition{}, err
		}
		currentTarget = status
	}

	completed, failed, canceled, total, err := targetSummaryTx(ctx, tx, taskID)
	if err != nil {
		return TargetTransition{}, err
	}
	transition := TargetTransition{TargetStatus: currentTarget}
	if total == 0 || completed+failed+canceled < total {
		return transition, tx.Commit()
	}
	transition.TaskStatus, transition.Detail = aggregateTaskStatus(completed, failed, canceled, total)
	transition.TaskTerminal = true
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET status=?,error=?,finished_at=?
		WHERE id=? AND status IN ('queued','running')`, transition.TaskStatus, transition.Detail, now(), taskID); err != nil {
		return TargetTransition{}, err
	}
	return transition, tx.Commit()
}

func targetSummaryTx(ctx context.Context, tx *sql.Tx, taskID string) (completed, failed, canceled, total int, err error) {
	rows, err := tx.QueryContext(ctx, `SELECT status,COUNT(*) FROM task_targets WHERE task_id=? GROUP BY status`, taskID)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return 0, 0, 0, 0, err
		}
		total += count
		switch status {
		case "completed":
			completed += count
		case "failed":
			failed += count
		case "canceled":
			canceled += count
		}
	}
	return completed, failed, canceled, total, rows.Err()
}

func aggregateTaskStatus(completed, failed, canceled, total int) (string, string) {
	if canceled == total {
		return "canceled", ""
	}
	if failed == total {
		return "failed", taskDetailAllClientsFailed
	}
	if failed > 0 || canceled > 0 {
		return "partial", taskDetailSomeClientsIncomplete
	}
	return "completed", ""
}

func isTargetTerminal(status string) bool {
	return status == "completed" || status == "failed" || status == "canceled"
}

func isTaskTerminal(status string) bool {
	return status == "completed" || status == "partial" || status == "failed" || status == "canceled"
}
