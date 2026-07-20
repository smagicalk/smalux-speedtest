//go:build with_utls

package clientapp

import (
	"context"
	"encoding/json"
	"testing"
)

// TestRealityOutboundInitializesWithUTLS guards the Client build contract used by
// Makefile. Reality is parsed even when sing-box is built without uTLS, but such a
// binary fails only when a task initializes its outbound. Starting a minimal box
// here catches that late runtime failure during the tagged test suite.
//
// The test does not dial the placeholder server. startBox only constructs and
// starts the outbound, which is enough to make sing-box select its Reality/uTLS
// implementation and validate the public key, short ID, and fingerprint.
func TestRealityOutboundInitializesWithUTLS(t *testing.T) {
	outbound := json.RawMessage(`{
		"type": "vless",
		"tag": "proxy",
		"server": "127.0.0.1",
		"server_port": 443,
		"uuid": "00000000-0000-0000-0000-000000000001",
		"tls": {
			"enabled": true,
			"server_name": "example.com",
			"utls": {
				"enabled": true,
				"fingerprint": "chrome"
			},
			"reality": {
				"enabled": true,
				"public_key": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
				"short_id": "abcd"
			}
		}
	}`)

	instance, _, err := startBox(context.Background(), outbound)
	if err != nil {
		t.Fatalf("initialize Reality outbound with uTLS: %v", err)
	}
	defer instance.Close()
}
