package subscription

import (
	"net/netip"
	"net/url"
	"testing"
)

func TestValidateURLBlocksUnsafeDestinations(t *testing.T) {
	blocked := []string{"file:///tmp/sub", "http://127.0.0.1/sub", "http://[::1]/sub", "http://user:pass@example.com/sub"}
	for _, raw := range blocked {
		parsed, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateURL(parsed); err == nil {
			t.Fatalf("expected %s to be blocked", raw)
		}
	}
	parsed, _ := url.Parse("https://example.com/sub")
	if err := validateURL(parsed); err != nil {
		t.Fatalf("public HTTPS URL was rejected: %v", err)
	}
}

func TestPublicAddress(t *testing.T) {
	if publicAddress(netip.MustParseAddr("10.0.0.1")) || publicAddress(netip.MustParseAddr("169.254.1.1")) {
		t.Fatal("private address accepted")
	}
	if !publicAddress(netip.MustParseAddr("1.1.1.1")) {
		t.Fatal("public address rejected")
	}
}
