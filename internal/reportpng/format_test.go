package reportpng

import (
	"testing"
	"time"
)

func TestFormatTaskTimeDoesNotEchoInvalidText(t *testing.T) {
	for _, value := range []string{"", "vless://user:secret@private.example:443", "/srv/private/report.db"} {
		if got := formatTaskTime(value); got != "-" {
			t.Fatalf("formatTaskTime(%q) = %q, want placeholder", value, got)
		}
	}

	value := time.Date(2026, 7, 20, 12, 34, 56, 0, time.FixedZone("test", 8*60*60)).Format(time.RFC3339Nano)
	if got := formatTaskTime(value); got != "2026-07-20 04:34:56 UTC" {
		t.Fatalf("formatTaskTime(valid) = %q", got)
	}
}
