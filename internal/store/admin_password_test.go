package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestChangeAdminPassword(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "admin-password.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.BootstrapAdmin(t.Context(), "initial-password"); err != nil {
		t.Fatal(err)
	}
	admin, err := database.AuthenticateAdmin(t.Context(), "admin", "initial-password")
	if err != nil {
		t.Fatal(err)
	}

	if err := database.ChangeAdminPassword(t.Context(), admin.ID, "wrong-password", "replacement-password"); !errors.Is(err, ErrInvalidAdminCredentials) {
		t.Fatalf("wrong current password error = %v", err)
	}
	if err := database.ChangeAdminPassword(t.Context(), admin.ID, "initial-password", "short"); !errors.Is(err, ErrInvalidAdminPassword) {
		t.Fatalf("invalid new password error = %v", err)
	}
	if err := database.ChangeAdminPassword(t.Context(), admin.ID, "initial-password", "initial-password"); !errors.Is(err, ErrAdminPasswordUnchanged) {
		t.Fatalf("unchanged password error = %v", err)
	}
	if err := database.ChangeAdminPassword(t.Context(), admin.ID, "initial-password", "replacement-password"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.AuthenticateAdmin(t.Context(), "admin", "initial-password"); !errors.Is(err, ErrInvalidAdminCredentials) {
		t.Fatalf("old password authentication error = %v", err)
	}
	if _, err := database.AuthenticateAdmin(t.Context(), "admin", "replacement-password"); err != nil {
		t.Fatalf("new password authentication error = %v", err)
	}
}
