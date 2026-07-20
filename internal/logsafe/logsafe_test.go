package logsafe

import (
	"errors"
	"strings"
	"testing"
)

func TestErrorTypeDoesNotIncludeMessage(t *testing.T) {
	const secret = "vless://uuid:password@192.0.2.10/private/path"
	value := ErrorType(errors.New(secret))
	if strings.Contains(value, secret) || strings.Contains(value, "192.0.2.10") {
		t.Fatalf("error type exposed message: %q", value)
	}
}
