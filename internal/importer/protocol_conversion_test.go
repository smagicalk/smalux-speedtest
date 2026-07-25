package importer

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

// TestProtocolConversions guards the share-link to sing-box boundary. These
// checks use synthetic endpoints and credentials; no connection is attempted.
func TestProtocolConversions(t *testing.T) {
	ssCredential := base64.RawURLEncoding.EncodeToString([]byte("aes-256-gcm:ss-password"))
	ssPercentCredential := base64.RawURLEncoding.EncodeToString([]byte("aes-128-gcm:literal%2Fpassword"))
	vmessJSON := `{"v":"2","ps":"vmess","add":"example.com","port":"443","id":"00000000-0000-0000-0000-000000000001","scy":"auto","net":"ws","path":"/vmess","host":"cdn.example.com","tls":"tls","sni":"example.com","packetEncoding":"packetaddr"}`
	vmessLink := "vmess://" + base64.RawStdEncoding.EncodeToString([]byte(vmessJSON))

	tests := []struct {
		name   string
		link   string
		assert func(*testing.T, map[string]any)
	}{
		{"vless-reality", "vless://00000000-0000-0000-0000-000000000001@example.com:443?security=reality&sni=front.example.com&fp=edge&pbk=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA&sid=0123456789abcdef&flow=xtls-rprx-vision&packetEncoding=xudp", assertVLESSReality},
		{"vless-grpc", "vless://00000000-0000-0000-0000-000000000001@example.com:443?security=tls&type=grpc&serviceName=speed", func(t *testing.T, out map[string]any) {
			transport := nestedMap(t, out, "transport")
			assertEqual(t, transport, "type", "grpc")
			assertEqual(t, transport, "service_name", "speed")
		}},
		{"hysteria2", "hy2://hy-password@example.com:443?sni=front.example.com&obfs=salamander&obfs-password=obfs-secret&insecure=1", assertHysteria2},
		{"hysteria2-port-hopping", "hy2://hy-password@example.com:443?mport=20000-20010%2C30000&hop-interval=15s", func(t *testing.T, out map[string]any) {
			assertEqual(t, out, "server_ports", []any{"20000:20010", "30000:30000"})
			assertEqual(t, out, "hop_interval", "15s")
		}},
		{"hysteria2-alias", "hysteria2://hy-password@example.com:443?sni=front.example.com", func(t *testing.T, out map[string]any) {
			assertEqual(t, out, "type", "hysteria2")
			assertEqual(t, out, "password", "hy-password")
		}},
		{"hysteria-v1", "hysteria://example.com:443?auth=hy1-password&peer=front.example.com&upmbps=100&downmbps=200&obfs=xplus&obfsParam=obfs-secret", assertHysteria},
		{"shadowsocks-base64", "ss://" + ssCredential + "@example.com:8388?plugin=obfs-local%3Bobfs%3Dtls%3Bobfs-host%3Dcdn.example.com", assertShadowsocks},
		{"shadowsocks-base64-percent", "ss://" + ssPercentCredential + "@example.com:8388", func(t *testing.T, out map[string]any) {
			assertEqual(t, out, "password", "literal%2Fpassword")
		}},
		{"shadowsocks-plain", "ss://aes-128-gcm:p%40ss%3Aword@example.com:8388", func(t *testing.T, out map[string]any) {
			assertEqual(t, out, "method", "aes-128-gcm")
			assertEqual(t, out, "password", "p@ss:word")
		}},
		{"vmess", vmessLink, assertVMess},
		{"trojan", "trojan://password@example.com:443?sni=front.example.com&type=ws&path=%2Ftrojan&host=cdn.example.com&ed=2048&eh=Sec-WebSocket-Protocol", assertTrojan},
		{"tuic", "tuic://00000000-0000-0000-0000-000000000001:password@example.com:443?sni=front.example.com&congestion_control=bbr&udp_relay_mode=native", assertTUIC},
		{"anytls", "anytls://password@example.com:443?sni=front.example.com&insecure=1", assertAnyTLS},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			proxy, err := ParseLink(test.link)
			if err != nil {
				t.Fatalf("parse link: %v", err)
			}
			var outbound map[string]any
			if err := json.Unmarshal(proxy.Outbound, &outbound); err != nil {
				t.Fatalf("decode outbound: %v", err)
			}
			test.assert(t, outbound)
		})
	}
}

func TestHysteriaV1RequiresBandwidth(t *testing.T) {
	_, err := ParseLink("hysteria://example.com:443?auth=password")
	if err == nil {
		t.Fatal("expected missing Hysteria v1 bandwidth to be rejected")
	}
}

func TestUnsupportedV2RayTransportIsRejected(t *testing.T) {
	vmessJSON := "{\"v\":\"2\",\"add\":\"example.com\",\"port\":\"443\",\"id\":\"00000000-0000-0000-0000-000000000001\",\"net\":\"kcp\"}"
	vmessLink := "vmess://" + base64.RawStdEncoding.EncodeToString([]byte(vmessJSON))
	tests := []string{
		"vless://00000000-0000-0000-0000-000000000001@example.com:443?type=kcp",
		"vless://00000000-0000-0000-0000-000000000001@example.com:443?security=tls&type=quic",
		"trojan://password@example.com:443?type=kcp",
		vmessLink,
	}
	for _, link := range tests {
		if _, err := ParseLink(link); err == nil {
			t.Fatalf("ParseLink accepted unsupported transport")
		}
	}
}

func TestInvalidHysteriaPortRangeIsRejected(t *testing.T) {
	_, err := ParseLink("hy2://password@example.com:443?mport=20010-20000")
	if err == nil {
		t.Fatal("expected descending Hysteria port range to be rejected")
	}
}

func assertHysteria(t *testing.T, out map[string]any) {
	assertEqual(t, out, "type", "hysteria")
	assertEqual(t, out, "auth_str", "hy1-password")
	assertEqual(t, out, "up_mbps", float64(100))
	assertEqual(t, out, "down_mbps", float64(200))
	assertEqual(t, out, "obfs", "obfs-secret")
	assertEqual(t, nestedMap(t, out, "tls"), "server_name", "front.example.com")
}

func assertVLESSReality(t *testing.T, out map[string]any) {
	assertEqual(t, out, "type", "vless")
	assertEqual(t, out, "flow", "xtls-rprx-vision")
	assertEqual(t, out, "packet_encoding", "xudp")
	tls := nestedMap(t, out, "tls")
	assertEqual(t, tls, "server_name", "front.example.com")
	assertEqual(t, nestedMap(t, tls, "utls"), "fingerprint", "edge")
	reality := nestedMap(t, tls, "reality")
	assertEqual(t, reality, "public_key", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	assertEqual(t, reality, "short_id", "0123456789abcdef")
}

func assertHysteria2(t *testing.T, out map[string]any) {
	assertEqual(t, out, "type", "hysteria2")
	assertEqual(t, out, "password", "hy-password")
	assertEqual(t, nestedMap(t, out, "obfs"), "type", "salamander")
	assertEqual(t, nestedMap(t, out, "obfs"), "password", "obfs-secret")
	tls := nestedMap(t, out, "tls")
	assertEqual(t, tls, "server_name", "front.example.com")
	assertEqual(t, tls, "insecure", true)
}

func assertShadowsocks(t *testing.T, out map[string]any) {
	assertEqual(t, out, "type", "shadowsocks")
	assertEqual(t, out, "method", "aes-256-gcm")
	assertEqual(t, out, "password", "ss-password")
	assertEqual(t, out, "plugin", "obfs-local")
	assertEqual(t, out, "plugin_opts", "obfs=tls;obfs-host=cdn.example.com")
}

func assertVMess(t *testing.T, out map[string]any) {
	assertEqual(t, out, "type", "vmess")
	assertEqual(t, out, "packet_encoding", "packetaddr")
	transport := nestedMap(t, out, "transport")
	assertEqual(t, transport, "type", "ws")
	assertEqual(t, transport, "path", "/vmess")
}

func assertTrojan(t *testing.T, out map[string]any) {
	assertEqual(t, out, "type", "trojan")
	transport := nestedMap(t, out, "transport")
	assertEqual(t, transport, "max_early_data", float64(2048))
	assertEqual(t, transport, "early_data_header_name", "Sec-WebSocket-Protocol")
}

func assertTUIC(t *testing.T, out map[string]any) {
	assertEqual(t, out, "type", "tuic")
	assertEqual(t, out, "congestion_control", "bbr")
	assertEqual(t, out, "udp_relay_mode", "native")
	assertEqual(t, nestedMap(t, out, "tls"), "server_name", "front.example.com")
}

func assertAnyTLS(t *testing.T, out map[string]any) {
	assertEqual(t, out, "type", "anytls")
	assertEqual(t, out, "password", "password")
	assertEqual(t, nestedMap(t, out, "tls"), "insecure", true)
}

func nestedMap(t *testing.T, parent map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := parent[key].(map[string]any)
	if !ok {
		t.Fatalf("%s is %T, want object: %#v", key, parent[key], parent)
	}
	return value
}

func assertEqual(t *testing.T, object map[string]any, key string, want any) {
	t.Helper()
	got := object[key]
	if !jsonValuesEqual(got, want) {
		t.Fatalf("%s = %#v, want %#v", key, got, want)
	}
}

func jsonValuesEqual(left, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}
