package store

import (
	"context"
	"database/sql"
	"errors"

	"golang.org/x/crypto/bcrypt"

	"smalux-speedtest/internal/model"
)

// CreateAdminUser 创建启用账户，数据库只接收 bcrypt 哈希。
func (s *Store) CreateAdminUser(ctx context.Context, username, password string) (AdminUser, error) {
	canonical, err := normalizeAdminUsername(username)
	if err != nil {
		return AdminUser{}, err
	}
	if err := validateAdminPassword(password); err != nil {
		return AdminUser{}, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return AdminUser{}, err
	}
	timestamp := now()
	user := AdminUser{ID: model.NewID(), Username: canonical, Enabled: true, CreatedAt: timestamp, UpdatedAt: timestamp}
	result, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO admin_users
		(id,username,password_hash,enabled,created_at,updated_at,last_login_at)
		VALUES(?,?,?,1,?,?,?)`, user.ID, user.Username, string(hash), timestamp, timestamp, "")
	if err != nil {
		return AdminUser{}, err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return AdminUser{}, err
	}
	if inserted != 1 {
		return AdminUser{}, ErrAdminUsernameTaken
	}
	return user, nil
}

// ListAdminUsers 返回账户公开元数据，绝不选择 password_hash。
func (s *Store) ListAdminUsers(ctx context.Context) ([]AdminUser, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,username,enabled,created_at,updated_at,last_login_at
		FROM admin_users ORDER BY username COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	users := make([]AdminUser, 0)
	for rows.Next() {
		var user AdminUser
		var enabled int
		if err := rows.Scan(&user.ID, &user.Username, &enabled, &user.CreatedAt, &user.UpdatedAt, &user.LastLoginAt); err != nil {
			return nil, err
		}
		user.Enabled = enabled != 0
		user.CreatedAt = normalizePersistedTimestamp(user.CreatedAt)
		user.UpdatedAt = normalizePersistedTimestamp(user.UpdatedAt)
		user.LastLoginAt = normalizePersistedTimestamp(user.LastLoginAt)
		users = append(users, user)
	}
	return users, rows.Err()
}

// SetAdminUserEnabled 在事务内保护最后一个启用账户。
func (s *Store) SetAdminUserEnabled(ctx context.Context, id string, enabled bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current int
	err = tx.QueryRowContext(ctx, `SELECT enabled FROM admin_users WHERE id=?`, id).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrAdminUserNotFound
	}
	if err != nil {
		return err
	}
	if !enabled && current != 0 {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM admin_users WHERE enabled=1`).Scan(&count); err != nil {
			return err
		}
		if count <= 1 {
			return ErrAdminUserLastEnabled
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE admin_users SET enabled=?,updated_at=? WHERE id=?`, boolInt(enabled), now(), id); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteAdminUser 删除账户，并在事务内保护最后一个启用账户。
func (s *Store) DeleteAdminUser(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var enabled int
	err = tx.QueryRowContext(ctx, `SELECT enabled FROM admin_users WHERE id=?`, id).Scan(&enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrAdminUserNotFound
	}
	if err != nil {
		return err
	}
	if enabled != 0 {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM admin_users WHERE enabled=1`).Scan(&count); err != nil {
			return err
		}
		if count <= 1 {
			return ErrAdminUserLastEnabled
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM admin_users WHERE id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}
