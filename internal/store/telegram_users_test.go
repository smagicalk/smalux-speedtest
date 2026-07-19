package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

// TestTelegramAuthorizationLifecycle 覆盖 owner 同步、普通授权更新、重启持久化、列表
// 排序和撤销语义，并特别验证普通 API 无法降级或删除 owner。
func TestTelegramAuthorizationLifecycle(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "telegram.db")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	authorized, err := database.IsTelegramAuthorized(ctx, 1001)
	if err != nil || authorized {
		t.Fatalf("unknown user authorization = %v, %v", authorized, err)
	}
	owner, err := database.SyncTelegramOwner(ctx, 1001, "owner", "Owner User")
	if err != nil {
		t.Fatal(err)
	}
	if !owner.Owner || owner.TelegramID != 1001 || owner.Username != "owner" {
		t.Fatalf("unexpected owner: %+v", owner)
	}
	isOwner, err := database.IsTelegramOwner(ctx, owner.TelegramID)
	if err != nil || !isOwner {
		t.Fatalf("owner was not recognized: %v, %v", isOwner, err)
	}

	// 普通授权 owner 只允许刷新展示元数据，不能把 is_owner 改回零。
	owner, err = database.AuthorizeTelegramUser(ctx, owner.TelegramID, "owner-renamed", "Renamed Owner")
	if err != nil {
		t.Fatal(err)
	}
	if !owner.Owner || owner.Username != "owner-renamed" {
		t.Fatalf("ordinary authorization downgraded owner: %+v", owner)
	}
	if err := database.RevokeTelegramUser(ctx, owner.TelegramID); !errors.Is(err, ErrTelegramOwnerProtected) {
		t.Fatalf("owner revoke error = %v, want ErrTelegramOwnerProtected", err)
	}

	member, err := database.AuthorizeTelegramUser(ctx, 2002, "member", "Member User")
	if err != nil {
		t.Fatal(err)
	}
	createdAt := member.CreatedAt
	member, err = database.AuthorizeTelegramUser(ctx, member.TelegramID, "member-renamed", "Renamed Member")
	if err != nil {
		t.Fatal(err)
	}
	if member.Owner || member.Username != "member-renamed" || member.CreatedAt != createdAt {
		t.Fatalf("unexpected updated member: %+v", member)
	}
	users, err := database.ListTelegramUsers(ctx)
	if err != nil || len(users) != 2 || !users[0].Owner || users[1].Owner {
		t.Fatalf("unexpected authorization list: %+v, %v", users, err)
	}

	// 关闭并重新打开同一文件，证明授权信息来自 SQLite，而非进程内状态。
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	authorized, err = database.IsTelegramAuthorized(ctx, member.TelegramID)
	if err != nil || !authorized {
		t.Fatalf("member authorization was not persisted: %v, %v", authorized, err)
	}
	if err := database.RevokeTelegramUser(ctx, member.TelegramID); err != nil {
		t.Fatal(err)
	}
	authorized, err = database.IsTelegramAuthorized(ctx, member.TelegramID)
	if err != nil || authorized {
		t.Fatalf("revoked user authorization = %v, %v", authorized, err)
	}
	if err := database.RevokeTelegramUser(ctx, member.TelegramID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("second revoke error = %v, want sql.ErrNoRows", err)
	}
	if _, err := database.IsTelegramAuthorized(ctx, 0); !errors.Is(err, ErrInvalidTelegramUserID) {
		t.Fatalf("invalid ID error = %v, want ErrInvalidTelegramUserID", err)
	}
}

// TestSyncTelegramOwnerReplacesPreviousOwner 验证启动配置变化时旧 owner 被撤销、已有普通
// 用户可原地提升为新 owner，并且其他普通授权不受 owner 切换影响。
func TestSyncTelegramOwnerReplacesPreviousOwner(t *testing.T) {
	ctx := context.Background()
	database, err := Open(filepath.Join(t.TempDir(), "owner-switch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.SyncTelegramOwner(ctx, 1001, "old-owner", "Old Owner"); err != nil {
		t.Fatal(err)
	}
	promoted, err := database.AuthorizeTelegramUser(ctx, 2002, "new-owner", "New Owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.AuthorizeTelegramUser(ctx, 3003, "member", "Member"); err != nil {
		t.Fatal(err)
	}
	promotedCreatedAt := promoted.CreatedAt
	promoted, err = database.SyncTelegramOwner(ctx, promoted.TelegramID, "new-owner-synced", "New Owner Synced")
	if err != nil {
		t.Fatal(err)
	}
	if !promoted.Owner || promoted.CreatedAt != promotedCreatedAt {
		t.Fatalf("existing member was not promoted in place: %+v", promoted)
	}
	oldAuthorized, err := database.IsTelegramAuthorized(ctx, 1001)
	if err != nil || oldAuthorized {
		t.Fatalf("old owner authorization = %v, %v", oldAuthorized, err)
	}
	users, err := database.ListTelegramUsers(ctx)
	if err != nil || len(users) != 2 || users[0].TelegramID != promoted.TelegramID || !users[0].Owner || users[1].TelegramID != 3003 {
		t.Fatalf("unexpected users after owner switch: %+v, %v", users, err)
	}
}

// TestOpenAddsTelegramAuthorizationSchema 模拟从没有 Telegram 表的旧数据库升级，确认
// Open 的幂等迁移会创建授权 schema，且随后可以立即同步 owner。
func TestOpenAddsTelegramAuthorizationSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy-telegram.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	owner, err := database.SyncTelegramOwner(ctx, 1001, "owner", "Owner")
	if err != nil || !owner.Owner {
		t.Fatalf("telegram schema was not migrated: %+v, %v", owner, err)
	}
}
