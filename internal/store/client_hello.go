package store

import (
	"context"
	"database/sql"
	"net"
	"regexp"
	"strings"

	"smalux-speedtest/internal/model"
)

const maxClientVersionBytes = 32

var clientVersionPattern = regexp.MustCompile(`^(?:dev|test|[vV]?[0-9]{1,4}(?:\.[0-9]{1,4}){0,3}(?:[-_][A-Za-z0-9][A-Za-z0-9._-]{0,15})?)$`)

// UpdateClientHello persists only bounded runtime metadata from an authenticated
// Client. Name and Labels are administrator-owned identity fields established by
// CreateClient; a bearer-token holder must not be able to replace them with an
// outbound, subscription URL, credential or arbitrary labels during the handshake.
//
// The enabled predicate closes the interval between Upgrade authentication and this
// update: a token revoked in that interval cannot refresh its metadata or LastSeen.
func (s *Store) UpdateClientHello(ctx context.Context, id string, hello model.Hello) error {
	result, err := s.db.ExecContext(ctx, `UPDATE clients SET version=?,os=?,arch=?,last_seen=? WHERE id=? AND enabled=1`,
		normalizeClientVersion(hello.Version), normalizeClientOS(hello.OS), normalizeClientArch(hello.Arch), now(), id)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// normalizeClientVersion accepts the compact identifiers emitted by release builds
// (for example dev, v1.2.3 or 2026.07.20-rc1). Invalid input is discarded as a whole
// instead of partially stripping it, which could leave a recognizable credential or
// endpoint fragment in SQLite.
func normalizeClientVersion(value string) string {
	value = strings.TrimSpace(value)
	versionWithoutPrefix := strings.TrimPrefix(strings.TrimPrefix(value, "v"), "V")
	if value == "" || len(value) > maxClientVersionBytes || net.ParseIP(versionWithoutPrefix) != nil || !clientVersionPattern.MatchString(value) {
		return ""
	}
	return value
}

// normalizeClientOS and normalizeClientArch accept only values that runtime.GOOS and
// runtime.GOARCH can produce for Go 1.26. Lowercasing allows harmless operator casing
// differences while preventing paths, endpoints and arbitrary key-like strings.
func normalizeClientOS(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "aix", "android", "darwin", "dragonfly", "freebsd", "illumos", "ios", "js", "linux", "netbsd", "openbsd", "plan9", "solaris", "wasip1", "windows":
		return value
	default:
		return ""
	}
}

func normalizeClientArch(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "386", "amd64", "arm", "arm64", "loong64", "mips", "mips64", "mips64le", "mipsle", "ppc64", "ppc64le", "riscv64", "s390x", "wasm":
		return value
	default:
		return ""
	}
}
