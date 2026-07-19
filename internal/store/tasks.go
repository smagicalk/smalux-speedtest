package store

import (
	"context"
	"fmt"
)

// CreateTask 原子地写入任务摘要和全部目标 Client。
//
// tasks 与 task_targets 必须同时成功或同时失败，否则调度器可能看到一个没有目标的
// 任务，或看到引用不存在任务的目标。defer Rollback 在任意提前返回时清理事务；成功
// Commit 后再次 Rollback 是无害的。ClientCount 从 clientIDs 计算，避免信任调用方字段。
func (s *Store) CreateTask(ctx context.Context, task Task, clientIDs []string) error {
	// 兼容未显式设置线程数的旧调用方，并与 schema 的 DEFAULT 4 保持一致。
	if task.Threads == 0 {
		task.Threads = 4
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO tasks(id,status,candidate_count,top_n,threads,proxy_count,client_count,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		task.ID, task.Status, task.CandidateCount, task.TopN, task.Threads, task.ProxyCount, len(clientIDs), task.CreatedAt)
	if err != nil {
		return err
	}
	for _, clientID := range clientIDs {
		// 目标插入与 Enabled 检查处于同一事务；管理员并发撤销时，两次写事务
		// 会由 SQLite 串行化，避免为已经撤销的 Client 创建新目标。
		result, err := tx.ExecContext(ctx, `INSERT INTO task_targets(task_id,client_id,status)
			SELECT ?,id,'queued' FROM clients WHERE id=? AND enabled=1`, task.ID, clientID)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed == 0 {
			return fmt.Errorf("client %s does not exist or is revoked", clientID)
		}
	}
	return tx.Commit()
}

// SetTaskStatus 更新任务状态、说明及生命周期时间。
//
// started_at 只在第一次进入 running 且原值为空时写入；终态会写 finished_at。CASE
// 表达式使状态与时间在同一条 SQL 中更新，避免并发读取到不一致的中间状态。
func (s *Store) SetTaskStatus(ctx context.Context, id, status, detail string) error {
	startedAt := ""
	finishedAt := ""
	if status == "running" {
		startedAt = now()
	}
	if status == "completed" || status == "partial" || status == "failed" || status == "canceled" {
		finishedAt = now()
	}
	_, err := s.db.ExecContext(ctx, `UPDATE tasks SET status=?,error=?,started_at=CASE WHEN started_at='' AND ?<>'' THEN ? ELSE started_at END,finished_at=CASE WHEN ?<>'' THEN ? ELSE finished_at END WHERE id=?`,
		status, detail, startedAt, startedAt, finishedAt, finishedAt, id)
	return err
}

// SetTargetStatus 更新任务中某个 Client 的独立执行状态与错误说明。
func (s *Store) SetTargetStatus(ctx context.Context, taskID, clientID, status, detail string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE task_targets SET status=?,error=? WHERE task_id=? AND client_id=?`, status, detail, taskID, clientID)
	return err
}

// TargetSummary 聚合任务各目标 Client 的终态数量。
// total 包含 queued/running 等所有状态；具名的 completed、failed、canceled 只统计对应
// 终态，调用方可据此判断任务最终应为 completed、partial 还是 failed。
func (s *Store) TargetSummary(ctx context.Context, taskID string) (completed, failed, canceled, total int, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT status,COUNT(*) FROM task_targets WHERE task_id=? GROUP BY status`, taskID)
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

// ListTasks 按创建时间倒序返回最近任务。
// limit 异常时回落到 50，并以 200 为硬上限，避免管理 API 一次读取无界历史数据。
func (s *Store) ListTasks(ctx context.Context, limit int) ([]Task, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,status,candidate_count,top_n,threads,proxy_count,client_count,error,created_at,started_at,finished_at FROM tasks ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tasks []Task
	for rows.Next() {
		var task Task
		if err := rows.Scan(&task.ID, &task.Status, &task.CandidateCount, &task.TopN, &task.Threads, &task.ProxyCount, &task.ClientCount, &task.Error, &task.CreatedAt, &task.StartedAt, &task.FinishedAt); err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

// GetTask 按 ID 返回单个任务摘要；不存在时返回 sql.ErrNoRows。
func (s *Store) GetTask(ctx context.Context, id string) (Task, error) {
	var task Task
	err := s.db.QueryRowContext(ctx, `SELECT id,status,candidate_count,top_n,threads,proxy_count,client_count,error,created_at,started_at,finished_at FROM tasks WHERE id=?`, id).
		Scan(&task.ID, &task.Status, &task.CandidateCount, &task.TopN, &task.Threads, &task.ProxyCount, &task.ClientCount, &task.Error, &task.CreatedAt, &task.StartedAt, &task.FinishedAt)
	return task, err
}
