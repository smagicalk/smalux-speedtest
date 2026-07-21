package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestAdminUsersLifecycleAndLastEnabledInvariant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admins.db")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.BootstrapAdmin(t.Context(), "initial-password"); err != nil {
		t.Fatal(err)
	}
	admin, err := database.AuthenticateAdmin(t.Context(), "ADMIN", "initial-password")
	if err != nil || admin.Username != "admin" {
		t.Fatalf("bootstrap authentication = %+v, %v", admin, err)
	}
	if !admin.IsOwner {
		t.Fatal("bootstrap admin is not marked as owner")
	}
	if _, err := database.CreateAdminUser(t.Context(), "bad user", "valid-password"); !errors.Is(err, ErrInvalidAdminUsername) {
		t.Fatalf("invalid username error = %v", err)
	}
	if _, err := database.CreateAdminUser(t.Context(), "operator", "short"); !errors.Is(err, ErrInvalidAdminPassword) {
		t.Fatalf("invalid password error = %v", err)
	}
	if _, err := database.CreateAdminUser(t.Context(), "operator", strings.Repeat("x", 73)); !errors.Is(err, ErrInvalidAdminPassword) {
		t.Fatalf("bcrypt over-limit password error = %v", err)
	}
	operator, err := database.CreateAdminUser(t.Context(), "Operator", "operator-password")
	if err != nil || operator.Username != "operator" {
		t.Fatalf("create operator = %+v, %v", operator, err)
	}
	if _, err := database.CreateAdminUser(t.Context(), "OPERATOR", "another-password"); !errors.Is(err, ErrAdminUsernameTaken) {
		t.Fatalf("duplicate username error = %v", err)
	}

	// 两个并发停用请求最多只能有一个成功；事务内计数必须始终留下一个账户。
	var wait sync.WaitGroup
	start := make(chan struct{})
	errorsByID := make(map[string]error)
	var errorsMu sync.Mutex
	for _, id := range []string{admin.ID, operator.ID} {
		wait.Add(1)
		go func(id string) {
			defer wait.Done()
			<-start
			err := database.SetAdminUserEnabled(context.Background(), id, false)
			errorsMu.Lock()
			errorsByID[id] = err
			errorsMu.Unlock()
		}(id)
	}
	close(start)
	wait.Wait()
	users, err := database.ListAdminUsers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	enabled := 0
	for _, user := range users {
		if user.Enabled {
			enabled++
		}
	}
	if enabled != 1 {
		t.Fatalf("enabled administrators = %d, users=%+v errors=%+v", enabled, users, errorsByID)
	}
	lastEnabledErrors := 0
	for _, err := range errorsByID {
		if errors.Is(err, ErrAdminUserLastEnabled) || errors.Is(err, ErrAdminOwnerProtected) {
			lastEnabledErrors++
		} else if err != nil {
			t.Fatalf("unexpected concurrent disable error: %v", err)
		}
	}
	if lastEnabledErrors != 1 {
		t.Fatalf("last-enabled protections = %d, errors=%+v", lastEnabledErrors, errorsByID)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	// 重启后账户状态和数量保持不变，Bootstrap 不会覆盖现有账户。
	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err := second.BootstrapAdmin(t.Context(), "ignored-password"); err != nil {
		t.Fatal(err)
	}
	afterRestart, err := second.ListAdminUsers(t.Context())
	if err != nil || len(afterRestart) != 2 {
		t.Fatalf("administrators after restart = %+v, %v", afterRestart, err)
	}
}

func TestLegacyAdminHashMigratesOnceAndIsRemoved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-admin.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`CREATE TABLE settings (key TEXT PRIMARY KEY,value TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte("legacy-password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`INSERT INTO settings(key,value) VALUES('admin_password_hash',?)`, string(hash)); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.AuthenticateAdmin(t.Context(), "admin", "legacy-password"); err != nil {
		t.Fatalf("migrated administrator cannot log in: %v", err)
	}
	var count int
	if err := database.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM settings WHERE key='admin_password_hash'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("legacy administrator hash was retained in settings")
	}
	users, err := database.ListAdminUsers(t.Context())
	if err != nil || len(users) != 1 {
		t.Fatalf("migrated administrators = %+v, %v", users, err)
	}
	operator, err := database.CreateAdminUser(t.Context(), "operator", "operator-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.DeleteAdminUser(t.Context(), users[0].ID); !errors.Is(err, ErrAdminOwnerProtected) {
		t.Fatalf("owner deletion error = %v", err)
	}
	if err := database.DeleteAdminUser(t.Context(), operator.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	// Owner 不能被删除；删除普通账户后重启，旧 setting 也不能再创建额外账户。
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	afterRestart, err := reopened.ListAdminUsers(t.Context())
	if err != nil || len(afterRestart) != 1 || afterRestart[0].Username != "admin" || !afterRestart[0].IsOwner {
		t.Fatalf("administrator resurrected after restart: %+v, %v", afterRestart, err)
	}
}
