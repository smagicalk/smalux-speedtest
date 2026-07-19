package importer

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"testing"
)

func TestParseShadowsocksSIP002(t *testing.T) {
	credential := base64.RawURLEncoding.EncodeToString([]byte("aes-128-gcm:secret"))
	result := Parse(fmt.Sprintf("ss://%s@example.com:8388#tokyo", credential))
	if len(result.Errors) != 0 || len(result.Proxies) != 1 {
		t.Fatalf("unexpected parse result: %+v", result)
	}
	proxy := result.Proxies[0]
	if proxy.Protocol != "shadowsocks" || proxy.Name != "tokyo" || proxy.Server != "example.com" || proxy.Port != 8388 {
		t.Fatalf("unexpected proxy: %+v", proxy)
	}
	var outbound map[string]any
	if err := json.Unmarshal(proxy.Outbound, &outbound); err != nil {
		t.Fatal(err)
	}
	if outbound["method"] != "aes-128-gcm" || outbound["password"] != "secret" {
		t.Fatalf("unexpected outbound: %s", proxy.Outbound)
	}
}

func TestParseVLESSRealityWebSocket(t *testing.T) {
	proxy, err := ParseLink("vless://uuid@example.com:443?security=reality&sni=edge.example.com&pbk=public&sid=abcd&type=ws&path=%2Fws&host=cdn.example.com#edge")
	if err != nil {
		t.Fatal(err)
	}
	var outbound map[string]any
	if err := json.Unmarshal(proxy.Outbound, &outbound); err != nil {
		t.Fatal(err)
	}
	tlsConfig := outbound["tls"].(map[string]any)
	reality := tlsConfig["reality"].(map[string]any)
	transport := outbound["transport"].(map[string]any)
	if reality["public_key"] != "public" || transport["type"] != "ws" || transport["path"] != "/ws" {
		t.Fatalf("unexpected outbound: %s", proxy.Outbound)
	}
}

func TestParseVMessAndBase64Subscription(t *testing.T) {
	vmessJSON := `{"v":"2","ps":"vmess-node","add":"vm.example.com","port":"443","id":"00000000-0000-0000-0000-000000000001","scy":"auto","net":"ws","path":"/socket","tls":"tls","sni":"vm.example.com"}`
	vmess := "vmess://" + base64.RawStdEncoding.EncodeToString([]byte(vmessJSON))
	batch := vmess + "\nnot-a-link"
	encoded := base64.StdEncoding.EncodeToString([]byte(batch))
	result := Parse(encoded)
	if len(result.Proxies) != 1 || len(result.Errors) != 1 {
		t.Fatalf("expected one proxy and one error, got %+v", result)
	}
	if result.Proxies[0].Name != "vmess-node" || result.Errors[0].Line != 2 {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestUnsupportedProtocolsAreReportedPerLine(t *testing.T) {
	result := Parse("ssr://abc\nnaive+https://user:pass@example.com:443")
	if len(result.Proxies) != 0 || len(result.Errors) != 2 {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestTrojanDefaultsToTLS(t *testing.T) {
	proxy, err := ParseLink("trojan://password@example.com:443#trojan")
	if err != nil {
		t.Fatal(err)
	}
	var outbound map[string]any
	if err := json.Unmarshal(proxy.Outbound, &outbound); err != nil {
		t.Fatal(err)
	}
	tlsConfig, ok := outbound["tls"].(map[string]any)
	if !ok || tlsConfig["enabled"] != true || tlsConfig["server_name"] != "example.com" {
		t.Fatalf("trojan TLS is missing: %s", proxy.Outbound)
	}
}

func TestWrappedBase64Subscription(t *testing.T) {
	credential := base64.RawURLEncoding.EncodeToString([]byte("aes-128-gcm:secret"))
	content := fmt.Sprintf("ss://%s@example.com:8388#node", credential)
	encoded := base64.StdEncoding.EncodeToString([]byte(content))
	wrapped := encoded[:len(encoded)/2] + "\n" + encoded[len(encoded)/2:]
	result := Parse(wrapped)
	if len(result.Proxies) != 1 || len(result.Errors) != 0 {
		t.Fatalf("wrapped subscription was not decoded: %+v", result)
	}
}
