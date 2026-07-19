package store

import (
	"context"
	"database/sql"
	"fmt"
)

// CancelTask 原子地取消全部未终结目标和父任务。已取消任务按幂等成功处理；其他终态
// 返回 sql.ErrNoRows，使 Hub 不会把已经完成的任务误报为刚刚取消。
func (s *Store) CancelTask(ctx context.Context, taskID, detail string) error {
	return s.terminateTask(ctx, taskID, "canceled", detail)
}

// FailTask 原子地把整个活动任务及其未终结目标标记为 failed。
// Hub 在整体超时时使用此路径，避免逐目标更新暴露半终结快照。
func (s *Store) FailTask(ctx context.Context, taskID, detail string) error {
	return s.terminateTask(ctx, taskID, "failed", detail)
}

// terminateTask 是 CancelTask 与 FailTask 共用的事务实现。它先锁定并检查父任务终态，
// 再一次更新全部活动目标和父任务；任何一步失败都会回滚，读取者不会观察到半终结状态。
func (s *Store) terminateTask(ctx context.Context, taskID, terminalStatus, detail string) error {
	if terminalStatus != "canceled" && terminalStatus != "failed" {
		return fmt.Errorf("invalid task terminal status %q", terminalStatus)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id=?`, taskID).Scan(&current); err != nil {
		return err
	}
	if current == terminalStatus {
		return tx.Commit()
	}
	if isTaskTerminal(current) {
		return sql.ErrNoRows
	}
	if _, err := tx.ExecContext(ctx, `UPDATE task_targets SET status=?,error=?
		WHERE task_id=? AND status IN ('queued','running')`, terminalStatus, detail, taskID); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE tasks SET status=?,error=?,finished_at=?
		WHERE id=? AND status IN ('queued','running')`, terminalStatus, detail, now(), taskID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}
