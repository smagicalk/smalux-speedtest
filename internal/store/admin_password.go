package store

import (
	"context"

	"golang.org/x/crypto/bcrypt"
)

// ChangeAdminPassword verifies the current credential before replacing its bcrypt
// hash. The conditional UPDATE prevents two concurrent changes from silently
// overwriting one another after both requests have read the same old hash.
func (s *Store) ChangeAdminPassword(ctx context.Context, id, currentPassword, newPassword string) error {
	if err := validateAdminPassword(newPassword); err != nil {
		return err
	}

	var currentHash string
	var enabled int
	err := s.db.QueryRowContext(ctx, `SELECT password_hash,enabled FROM admin_users WHERE id=?`, id).Scan(&currentHash, &enabled)
	if err != nil || enabled == 0 {
		_ = bcrypt.CompareHashAndPassword([]byte(dummyAdminPasswordHash), []byte(currentPassword))
		return ErrInvalidAdminCredentials
	}
	if bcrypt.CompareHashAndPassword([]byte(currentHash), []byte(currentPassword)) != nil {
		return ErrInvalidAdminCredentials
	}
	if bcrypt.CompareHashAndPassword([]byte(currentHash), []byte(newPassword)) == nil {
		return ErrAdminPasswordUnchanged
	}

	newHash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE admin_users SET password_hash=?,updated_at=?
		WHERE id=? AND password_hash=? AND enabled=1`, string(newHash), now(), id, currentHash)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrInvalidAdminCredentials
	}
	return nil
}
