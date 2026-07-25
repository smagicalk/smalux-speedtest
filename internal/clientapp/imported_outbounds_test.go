//go:build with_utls

package clientapp

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"smalux-speedtest/internal/importer"
)

// TestImportedOutboundsInitialize verifies the boundary that matters for every
// supported share-link family: importer output must be accepted by the exact
// sing-box registry embedded in the Client. The placeholder endpoints are never
// dialed; startBox only validates and initializes each outbound implementation.
func TestImportedOutboundsInitialize(t *testing.T) {
	shadowsocksCredential := base64.RawURLEncoding.EncodeToString([]byte("aes-128-gcm:password"))
	vmessPayload := base64.RawStdEncoding.EncodeToString([]byte(`{"v":"2","ps":"vmess","add":"example.com","port":"443","id":"00000000-0000-0000-0000-000000000001","aid":"0","scy":"auto","net":"tcp","tls":"tls","sni":"example.com"}`))
	cases := []struct {
		name string
		link string
	}{
		{
			name: "vless-reality-vision",
			link: "vless://00000000-0000-0000-0000-000000000001@example.com:443?security=reality&sni=example.com&fp=edge&pbk=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA&sid=0123456789abcdef&flow=xtls-rprx-vision&packetEncoding=xudp",
		},
		{
			name: "hysteria2-salamander",
			link: "hy2://password@example.com:443?sni=example.com&obfs=salamander&obfs-password=obfs-password",
		},
		{name: "hysteria2-port-hopping", link: "hy2://password@example.com:443?mport=20000-20010%2C30000&hop-interval=15s"},
		{name: "hysteria-v1", link: "hysteria://example.com:443?auth=password&peer=example.com&upmbps=100&downmbps=100&obfs=xplus&obfsParam=obfs-password"},
		{name: "shadowsocks-aead", link: "ss://" + shadowsocksCredential + "@example.com:8388"},
		{name: "trojan-tls", link: "trojan://password@example.com:443?sni=example.com"},
		{name: "trojan-websocket-early-data", link: "trojan://password@example.com:443?sni=example.com&type=ws&path=%2Ftrojan&ed=2048&eh=Sec-WebSocket-Protocol"},
		{name: "tuic", link: "tuic://00000000-0000-0000-0000-000000000001:password@example.com:443?sni=example.com"},
		{name: "anytls", link: "anytls://password@example.com:443?sni=example.com"},
		{name: "vmess", link: "vmess://" + vmessPayload},
		{name: "socks5", link: "socks5://user:password@example.com:1080"},
		{name: "http", link: "http://user:password@example.com:8080"},
		{name: "ssh", link: "ssh://user:password@example.com:22"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			proxy, err := importer.ParseLink(test.link)
			if err != nil {
				t.Fatalf("parse share link: %v", err)
			}
			instance, _, err := startBox(context.Background(), proxy.Outbound)
			if err != nil {
				// Hosted sandboxes sometimes prohibit sing-box's route monitor even
				// though the outbound JSON and protocol registration are valid.
				if message := strings.ToLower(err.Error()); strings.Contains(message, "operation not permitted") && strings.Contains(message, "route") {
					t.Skip("sing-box route monitor is unavailable in this environment")
				}
				t.Fatalf("initialize imported outbound: %v", err)
			}
			defer instance.Close()
		})
	}
}
