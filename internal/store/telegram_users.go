package store

import (
	"context"
	"database/sql"
	"errors"
)

var (
	// ErrTelegramOwnerProtected 表示调用方试图通过普通撤销流程删除最高权限 owner。
	// owner 只能由下一次 SyncTelegramOwner 使用服务启动配置进行替换。
	ErrTelegramOwnerProtected = errors.New("telegram owner cannot be revoked")
	// ErrInvalidTelegramUserID 表示 Telegram 用户 ID 不是有效的正整数。
	ErrInvalidTelegramUserID = errors.New("telegram user ID must be positive")
)

// TelegramUser 是 Telegram Bot 的持久化授权主体。
//
// TelegramID 是唯一、稳定且用于权限判断的身份键；Username 和 DisplayName 都可能
// 被用户修改，只用于管理界面与 Bot 消息展示。Owner 表示由服务启动配置确定的唯一
// 最高权限用户，普通授权和撤销 API 都不能降低或删除该身份。
type TelegramUser struct {
	TelegramID  int64  `json:"telegram_id"`
	Username    string `json:"username,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	Owner       bool   `json:"owner"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

// SyncTelegramOwner 将服务启动配置中的 Telegram 用户同步为唯一 owner。
//
// 同步在事务内先删除不同 ID 的旧 owner，再插入或提升当前用户。这样配置切换后旧
// owner 不会继续保留普通授权，而部分唯一索引也不会出现短暂冲突。相同 ID 重复同步
// 只刷新展示元数据和 UpdatedAt，保留最初 CreatedAt，适合每次服务启动幂等调用。
func (s *Store) SyncTelegramOwner(ctx context.Context, telegramID int64, username, displayName string) (TelegramUser, error) {
	if telegramID <= 0 {
		return TelegramUser{}, ErrInvalidTelegramUserID
	}
	timestamp := now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TelegramUser{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM telegram_users WHERE is_owner=1 AND telegram_id<>?`, telegramID); err != nil {
		return TelegramUser{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO telegram_users(telegram_id,username,display_name,is_owner,created_at,updated_at)
		VALUES(?,?,?,1,?,?)
		ON CONFLICT(telegram_id) DO UPDATE SET username=excluded.username,display_name=excluded.display_name,is_owner=1,updated_at=excluded.updated_at`,
		telegramID, username, displayName, timestamp, timestamp); err != nil {
		return TelegramUser{}, err
	}
	if err := tx.Commit(); err != nil {
		return TelegramUser{}, err
	}
	return s.getTelegramUser(ctx, telegramID)
}

// IsTelegramAuthorized 查询用户是否位于 Telegram 授权表中。
// owner 本身也是授权用户；查询只依据不可变的 Telegram ID，不信任 username。
func (s *Store) IsTelegramAuthorized(ctx context.Context, telegramID int64) (bool, error) {
	if telegramID <= 0 {
		return false, ErrInvalidTelegramUserID
	}
	var authorized int
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM telegram_users WHERE telegram_id=?)`, telegramID).Scan(&authorized)
	return authorized != 0, err
}

// IsTelegramOwner 查询用户是否是服务启动配置同步的唯一 owner。
// 该方法供命令处理层区分普通授权命令和仅 owner 可执行的授权管理命令。
func (s *Store) IsTelegramOwner(ctx context.Context, telegramID int64) (bool, error) {
	if telegramID <= 0 {
		return false, ErrInvalidTelegramUserID
	}
	var owner int
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM telegram_users WHERE telegram_id=? AND is_owner=1)`, telegramID).Scan(&owner)
	return owner != 0, err
}

// AuthorizeTelegramUser 新增普通 Telegram 用户，或刷新已有用户的展示元数据。
//
// 冲突更新刻意不写 is_owner：若目标 ID 已是 owner，普通授权操作只更新用户名和显示
// 名称，绝不会把 owner 降级。返回值是写入后的完整持久化状态。
func (s *Store) AuthorizeTelegramUser(ctx context.Context, telegramID int64, username, displayName string) (TelegramUser, error) {
	if telegramID <= 0 {
		return TelegramUser{}, ErrInvalidTelegramUserID
	}
	timestamp := now()
	_, err := s.db.ExecContext(ctx, `INSERT INTO telegram_users(telegram_id,username,display_name,is_owner,created_at,updated_at)
		VALUES(?,?,?,0,?,?)
		ON CONFLICT(telegram_id) DO UPDATE SET username=excluded.username,display_name=excluded.display_name,updated_at=excluded.updated_at`,
		telegramID, username, displayName, timestamp, timestamp)
	if err != nil {
		return TelegramUser{}, err
	}
	return s.getTelegramUser(ctx, telegramID)
}

// RevokeTelegramUser 撤销一个普通 Telegram 用户的授权。
//
// DELETE 自身带 is_owner=0 条件，使 owner 即使在并发或上层漏检时也不会被删除。目标
// 不存在时返回 sql.ErrNoRows；目标是 owner 时返回 ErrTelegramOwnerProtected，调用方
// 可通过 errors.Is 区分并向管理员给出明确反馈。
func (s *Store) RevokeTelegramUser(ctx context.Context, telegramID int64) error {
	if telegramID <= 0 {
		return ErrInvalidTelegramUserID
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM telegram_users WHERE telegram_id=? AND is_owner=0`, telegramID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	var owner int
	err = s.db.QueryRowContext(ctx, `SELECT is_owner FROM telegram_users WHERE telegram_id=?`, telegramID).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return sql.ErrNoRows
	}
	if err != nil {
		return err
	}
	if owner != 0 {
		return ErrTelegramOwnerProtected
	}
	return sql.ErrNoRows
}

// ListTelegramUsers 返回完整 Telegram 授权白名单，owner 固定排在首位。
// 其余用户按授权创建时间和 ID 稳定排序，便于管理命令和界面直接展示。
func (s *Store) ListTelegramUsers(ctx context.Context) ([]TelegramUser, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT telegram_id,username,display_name,is_owner,created_at,updated_at
		FROM telegram_users ORDER BY is_owner DESC,created_at,telegram_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []TelegramUser
	for rows.Next() {
		user, err := scanTelegramUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, user)
	}
	return users, rows.Err()
}

// getTelegramUser 返回单个授权主体，主要用于写操作后读取数据库最终状态。
func (s *Store) getTelegramUser(ctx context.Context, telegramID int64) (TelegramUser, error) {
	return scanTelegramUser(s.db.QueryRowContext(ctx, `SELECT telegram_id,username,display_name,is_owner,created_at,updated_at
		FROM telegram_users WHERE telegram_id=?`, telegramID))
}

// scanTelegramUser 统一转换 sql.Row/sql.Rows 的整数布尔值与公开结构。
func scanTelegramUser(row scanner) (TelegramUser, error) {
	var user TelegramUser
	var owner int
	if err := row.Scan(&user.TelegramID, &user.Username, &user.DisplayName, &owner, &user.CreatedAt, &user.UpdatedAt); err != nil {
		return TelegramUser{}, err
	}
	user.Owner = owner != 0
	return user, nil
}
