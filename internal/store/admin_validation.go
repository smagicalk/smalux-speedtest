package store

import (
	"strings"
	"unicode"
)

// normalizeAdminUsername 只接受易于人工确认的 ASCII 用户名，且统一转小写。
func normalizeAdminUsername(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) < 3 || len(value) > 32 {
		return "", ErrInvalidAdminUsername
	}
	for index, char := range value {
		if index == 0 && (char < 'a' || char > 'z') {
			return "", ErrInvalidAdminUsername
		}
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '.' && char != '_' && char != '-' {
			return "", ErrInvalidAdminUsername
		}
	}
	return value, nil
}

// validateAdminPassword 同时遵守产品最小长度和 bcrypt 的 72 字节上限。
func validateAdminPassword(value string) error {
	if len([]rune(value)) < 8 || len(value) > 72 {
		return ErrInvalidAdminPassword
	}
	for _, char := range value {
		if unicode.IsControl(char) {
			return ErrInvalidAdminPassword
		}
	}
	return nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
