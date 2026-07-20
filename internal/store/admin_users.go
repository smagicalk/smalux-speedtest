package store

import (
	"context"
	"database/sql"
	"errors"

	"golang.org/x/crypto/bcrypt"

	"smalux-speedtest/internal/model"
)

// AdminUser 是管理端登录账户的公开元数据。
//
// 密码哈希、登录失败原因和其他认证材料永远不会进入这个结构，因此它可以
// 直接作为管理 API 的 JSON 响应。Username 经过严格的 ASCII 规范化，避免
// 同形字符和大小写差异造成账户混淆。
type AdminUser struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	Enabled     bool   `json:"enabled"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
	LastLoginAt string `json:"last_login_at,omitempty"`
}

var (
	// ErrInvalidAdminUsername/Password 用于创建账户时区分输入校验错误；登录
	// 始终使用 ErrInvalidAdminCredentials，避免泄漏账户是否存在或已禁用。
	ErrInvalidAdminUsername    = errors.New("invalid administrator username")
	ErrInvalidAdminPassword    = errors.New("invalid administrator password")
	ErrInvalidAdminCredentials = errors.New("invalid administrator credentials")
	ErrAdminUsernameTaken      = errors.New("administrator username is already in use")
	ErrAdminUserNotFound       = errors.New("administrator user not found")
	ErrAdminUserLastEnabled    = errors.New("cannot disable the last enabled administrator")
)

const (
	// legacyAdminID 只用于从旧 settings 表迁移时保持幂等。它不承载秘密，也不
	// 被复用为任务或 Client ID。
	legacyAdminID = "admin-legacy"
	// bcrypt 的固定有效哈希用于不存在账户的登录尝试，尽量让查询失败路径也
	// 消耗与真实 CompareHashAndPassword 接近的时间。它不对应任何部署密码。
	dummyAdminPasswordHash = "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"
)

// migrateAdminUsers 创建管理员表，并把旧版本单一 admin_password_hash 转换
// 为名为 admin 的账户。迁移后删除旧 setting，避免该账户被删除后在下次
// 启动时因遗留哈希而重新出现。
//
// 该函数由 Store.migrate 调用；使用 INSERT OR IGNORE 保证重复启动不会覆盖
// 管理员自行修改过的账户或密码。
func (s *Store) migrateAdminUsers(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS admin_users (
		id TEXT PRIMARY KEY,
		username TEXT NOT NULL COLLATE NOCASE UNIQUE,
		password_hash TEXT NOT NULL,
		enabled INTEGER NOT NULL DEFAULT 1 CHECK(enabled IN (0,1)),
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		last_login_at TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		return err
	}
	var hash string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='admin_password_hash'`).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// 只迁移结构合法的 bcrypt 哈希。损坏值同样会被删除，随后由
	// BootstrapAdmin 使用显式启动密码创建一个可登录账户。
	if _, costErr := bcrypt.Cost([]byte(hash)); costErr == nil {
		timestamp := now()
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO admin_users
			(id,username,password_hash,enabled,created_at,updated_at,last_login_at)
			VALUES(?,?,?,1,?,?,?)`, legacyAdminID, "admin", hash, timestamp, timestamp, ""); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM settings WHERE key='admin_password_hash'`); err != nil {
		return err
	}
	return tx.Commit()
}

// BootstrapAdmin 在数据库还没有任何管理员时创建默认 admin 账户。
//
// 首次安装仍使用 SMALUX_ADMIN_PASSWORD；旧 settings 哈希已经由
// migrateAdminUsers 一次性转换，因此这里不会保留第二份可复活账户的凭据。
func (s *Store) BootstrapAdmin(ctx context.Context, password string) error {
	if err := s.migrateAdminUsers(ctx); err != nil {
		return err
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM admin_users`).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}

	if err := validateAdminPassword(password); err != nil {
		return errors.New("SMALUX_ADMIN_PASSWORD must contain at least 8 characters and at most 72 bytes on first start")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	timestamp := now()
	_, err = s.db.ExecContext(ctx, `INSERT INTO admin_users
		(id,username,password_hash,enabled,created_at,updated_at,last_login_at)
		VALUES(?,?,?,1,?,?,?)`, model.NewID(), "admin", string(hash), timestamp, timestamp, "")
	return err
}
