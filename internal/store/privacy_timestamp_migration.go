package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// scrubLegacyTimestamps removes arbitrary text that older direct callers or a
// restored database could contain in report-visible timestamp columns. Mandatory
// creation times receive one server-owned fallback value; optional lifecycle times
// become empty when they cannot be parsed as RFC3339Nano.
func (s *Store) scrubLegacyTimestamps(ctx context.Context) error {
	var marker string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, privacyTimestampScrubSetting).Scan(&marker)
	if err == nil && marker == "complete" {
		return nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read timestamp scrub marker: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	fallback := now()

	type taskTimes struct {
		id, createdAt, startedAt, finishedAt string
	}
	taskRows, err := tx.QueryContext(ctx, `SELECT id,created_at,started_at,finished_at FROM tasks`)
	if err != nil {
		return fmt.Errorf("read legacy task timestamps: %w", err)
	}
	var tasks []taskTimes
	for taskRows.Next() {
		var item taskTimes
		if err := taskRows.Scan(&item.id, &item.createdAt, &item.startedAt, &item.finishedAt); err != nil {
			taskRows.Close()
			return fmt.Errorf("scan legacy task timestamps: %w", err)
		}
		tasks = append(tasks, item)
	}
	if err := taskRows.Err(); err != nil {
		taskRows.Close()
		return fmt.Errorf("iterate legacy task timestamps: %w", err)
	}
	if err := taskRows.Close(); err != nil {
		return fmt.Errorf("close legacy task timestamps: %w", err)
	}
	for _, item := range tasks {
		createdAt := normalizePersistedTimestamp(item.createdAt)
		if createdAt == "" {
			createdAt = fallback
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET created_at=?,started_at=?,finished_at=? WHERE id=?`,
			createdAt, normalizePersistedTimestamp(item.startedAt), normalizePersistedTimestamp(item.finishedAt), item.id); err != nil {
			return fmt.Errorf("scrub legacy task timestamps: %w", err)
		}
	}

	type resultTime struct {
		id, createdAt string
	}
	resultRows, err := tx.QueryContext(ctx, `SELECT id,created_at FROM results`)
	if err != nil {
		return fmt.Errorf("read legacy result timestamps: %w", err)
	}
	var results []resultTime
	for resultRows.Next() {
		var item resultTime
		if err := resultRows.Scan(&item.id, &item.createdAt); err != nil {
			resultRows.Close()
			return fmt.Errorf("scan legacy result timestamps: %w", err)
		}
		results = append(results, item)
	}
	if err := resultRows.Err(); err != nil {
		resultRows.Close()
		return fmt.Errorf("iterate legacy result timestamps: %w", err)
	}
	if err := resultRows.Close(); err != nil {
		return fmt.Errorf("close legacy result timestamps: %w", err)
	}
	for _, item := range results {
		createdAt := normalizePersistedTimestamp(item.createdAt)
		if createdAt == "" {
			createdAt = fallback
		}
		if _, err := tx.ExecContext(ctx, `UPDATE results SET created_at=? WHERE id=?`, createdAt, item.id); err != nil {
			return fmt.Errorf("scrub legacy result timestamps: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES(?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, privacyTimestampScrubSetting, "complete"); err != nil {
		return fmt.Errorf("write timestamp scrub marker: %w", err)
	}
	return tx.Commit()
}
