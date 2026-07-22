package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"smalux-speedtest/internal/model"
)

// AdminInvite 是 Owner 在设置页生成的邀请码公开元数据。
//
// Code 不会持久化到数据库，只会在 CreateAdminInvite 的返回值中出现一次。
// 列表接口仅展示使用/吊销状态，避免页面刷新后仍能看到可用于注册的秘密。
type AdminInvite struct {
	ID               string `json:"id"`
	CreatedByAdminID string `json:"created_by_admin_id"`
	UsedByAdminID    string `json:"used_by_admin_id,omitempty"`
	CreatedAt        string `json:"created_at"`
	UsedAt           string `json:"used_at,omitempty"`
	RevokedAt        string `json:"revoked_at,omitempty"`
}

var (
	ErrInvalidAdminInvite = errors.New("invalid administrator invite")
)

// CreateAdminInvite 生成一个一次性注册邀请码，并只返回一次明文 code。
func (s *Store) CreateAdminInvite(ctx context.Context, createdByAdminID string) (AdminInvite, string, error) {
	createdByAdminID = strings.TrimSpace(createdByAdminID)
	if createdByAdminID == "" {
		return AdminInvite{}, "", ErrAdminUserNotFound
	}
	for attempts := 0; attempts < 3; attempts++ {
		code, err := randomToken(24)
		if err != nil {
			return AdminInvite{}, "", err
		}
		code = "smx-" + code
		timestamp := now()
		invite := AdminInvite{ID: model.NewID(), CreatedByAdminID: createdByAdminID, CreatedAt: timestamp}
		result, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO admin_invites
			(id,code_hash,created_by_admin_id,created_at) VALUES(?,?,?,?)`,
			invite.ID, tokenHash(code), createdByAdminID, timestamp)
		if err != nil {
			return AdminInvite{}, "", err
		}
		inserted, err := result.RowsAffected()
		if err != nil {
			return AdminInvite{}, "", err
		}
		if inserted == 1 {
			return invite, code, nil
		}
	}
	return AdminInvite{}, "", errors.New("failed to allocate unique administrator invite")
}

// ListAdminInvites 返回最近的邀请码状态，不包含 code_hash 或明文 code。
func (s *Store) ListAdminInvites(ctx context.Context) ([]AdminInvite, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,created_by_admin_id,used_by_admin_id,created_at,used_at,revoked_at
		FROM admin_invites ORDER BY created_at DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	invites := make([]AdminInvite, 0)
	for rows.Next() {
		var invite AdminInvite
		if err := rows.Scan(&invite.ID, &invite.CreatedByAdminID, &invite.UsedByAdminID, &invite.CreatedAt, &invite.UsedAt, &invite.RevokedAt); err != nil {
			return nil, err
		}
		invite.CreatedAt = normalizePersistedTimestamp(invite.CreatedAt)
		invite.UsedAt = normalizePersistedTimestamp(invite.UsedAt)
		invite.RevokedAt = normalizePersistedTimestamp(invite.RevokedAt)
		invites = append(invites, invite)
	}
	return invites, rows.Err()
}

// RevokeAdminInvite 撤销尚未使用的邀请码。已使用邀请码保留历史状态，不能改回可用。
func (s *Store) RevokeAdminInvite(ctx context.Context, id string) error {
	if !validGeneratedID(id) {
		return ErrInvalidAdminInvite
	}
	result, err := s.db.ExecContext(ctx, `UPDATE admin_invites SET revoked_at=?
		WHERE id=? AND used_at='' AND revoked_at=''`, now(), id)
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated != 1 {
		return ErrInvalidAdminInvite
	}
	return nil
}

// RegisterAdminWithInvite 使用一次性邀请码创建普通管理员。
//
// 用户名和密码先在事务外校验并生成 bcrypt 哈希，避免在写锁内执行昂贵计算。
// 邀请消费和账户创建仍在同一事务内完成，因此同一个邀请码的并发注册最多一个成功。
func (s *Store) RegisterAdminWithInvite(ctx context.Context, code, username, password string) (AdminUser, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return AdminUser{}, ErrInvalidAdminInvite
	}
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

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AdminUser{}, err
	}
	defer tx.Rollback()

	var inviteID, usedAt, revokedAt string
	err = tx.QueryRowContext(ctx, `SELECT id,used_at,revoked_at FROM admin_invites WHERE code_hash=?`, tokenHash(code)).
		Scan(&inviteID, &usedAt, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) || usedAt != "" || revokedAt != "" {
		return AdminUser{}, ErrInvalidAdminInvite
	}
	if err != nil {
		return AdminUser{}, err
	}

	timestamp := now()
	user := AdminUser{ID: model.NewID(), Username: canonical, Enabled: true, CreatedAt: timestamp, UpdatedAt: timestamp}
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO admin_users
		(id,username,password_hash,enabled,created_at,updated_at,last_login_at,is_owner)
		VALUES(?,?,?,1,?,?,?,0)`, user.ID, user.Username, string(hash), timestamp, timestamp, "")
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
	result, err = tx.ExecContext(ctx, `UPDATE admin_invites SET used_by_admin_id=?,used_at=?
		WHERE id=? AND used_at='' AND revoked_at=''`, user.ID, timestamp, inviteID)
	if err != nil {
		return AdminUser{}, err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return AdminUser{}, err
	}
	if updated != 1 {
		return AdminUser{}, ErrInvalidAdminInvite
	}
	if err := tx.Commit(); err != nil {
		return AdminUser{}, err
	}
	return user, nil
}
