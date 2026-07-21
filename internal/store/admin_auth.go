package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// AuthenticateAdmin 使用 bcrypt 校验管理员凭据，并更新成功登录时间。
// 所有查询失败、用户名不存在、密码错误和账户禁用都折叠为同一个错误。
func (s *Store) AuthenticateAdmin(ctx context.Context, username, password string) (AdminUser, error) {
	canonical, err := normalizeAdminUsername(username)
	if err != nil {
		_ = bcrypt.CompareHashAndPassword([]byte(dummyAdminPasswordHash), []byte(password))
		return AdminUser{}, ErrInvalidAdminCredentials
	}
	var user AdminUser
	var hash string
	var enabled, owner int
	err = s.db.QueryRowContext(ctx, `SELECT id,username,password_hash,enabled,is_owner,created_at,updated_at,last_login_at
		FROM admin_users WHERE username=? COLLATE NOCASE`, canonical).
		Scan(&user.ID, &user.Username, &hash, &enabled, &owner, &user.CreatedAt, &user.UpdatedAt, &user.LastLoginAt)
	if err != nil {
		hash = dummyAdminPasswordHash
	}
	passwordOK := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
	if err != nil || !passwordOK || enabled == 0 {
		return AdminUser{}, ErrInvalidAdminCredentials
	}
	user.Enabled = true
	user.IsOwner = owner != 0
	timestamp := now()
	result, updateErr := s.db.ExecContext(ctx, `UPDATE admin_users SET last_login_at=? WHERE id=? AND enabled=1`, timestamp, user.ID)
	if updateErr != nil {
		// 登录时间更新也复验 enabled，账户并发停用时不会签发新会话。
		return AdminUser{}, ErrInvalidAdminCredentials
	}
	changed, updateErr := result.RowsAffected()
	if updateErr != nil || changed != 1 {
		return AdminUser{}, ErrInvalidAdminCredentials
	}
	user.LastLoginAt = timestamp
	return user, nil
}

// VerifyAdmin 保留旧调用方的兼容入口，默认验证迁移后的 admin 用户。
func (s *Store) VerifyAdmin(ctx context.Context, password string) bool {
	_, err := s.AuthenticateAdmin(ctx, "admin", password)
	return err == nil
}

// IsAdminEnabled 让 HTTP 层在每个请求开始时确认会话账户仍可登录。
func (s *Store) IsAdminEnabled(ctx context.Context, id string) (bool, error) {
	if strings.TrimSpace(id) == "" {
		return false, nil
	}
	var enabled int
	err := s.db.QueryRowContext(ctx, `SELECT enabled FROM admin_users WHERE id=?`, id).Scan(&enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return enabled != 0, err
}
