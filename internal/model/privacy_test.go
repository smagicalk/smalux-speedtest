package model

import (
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeSensitiveErrors(t *testing.T) {
	if got := NormalizeResultError(ResultErrorDownload); got != ResultErrorDownload {
		t.Fatalf("safe result error changed to %q", got)
	}
	secret := "start sing-box: vless://uuid:password@secret.example:443"
	if got := NormalizeResultError(secret); got != ResultErrorProxyTest {
		t.Fatalf("untrusted result error normalized to %q", got)
	}
	if got := NormalizeTaskFailure("context deadline exceeded"); got != TaskFailureTimeout {
		t.Fatalf("deadline normalized to %q", got)
	}
	if got := NormalizeTaskFailure(secret); got != TaskFailureClient {
		t.Fatalf("untrusted task failure normalized to %q", got)
	}
}

func TestEraseAssignmentOverwritesOutbound(t *testing.T) {
	raw := []byte(`{"type":"vless","uuid":"secret-uuid"}`)
	assignment := Assignment{
		TaskID: "task", Proxies: []ProxySpec{{
			ID: "proxy", Name: "node", Server: "secret.example", Port: 443, Outbound: raw,
		}},
	}
	EraseAssignment(&assignment)
	if assignment.TaskID != "" || assignment.Proxies != nil {
		t.Fatalf("assignment retained fields: %+v", assignment)
	}
	for index, value := range raw {
		if value != 0 {
			t.Fatalf("outbound byte %d was not overwritten", index)
		}
	}
}

func TestNormalizeProxyNamePreservesNormalLabels(t *testing.T) {
	for _, test := range []struct {
		protocol string
		name     string
		server   string
		want     string
	}{
		{protocol: "vless", name: "Tokyo 01", server: "edge.example.com", want: "Tokyo 01"},
		{protocol: "shadowsocks", name: "[JP] zouter", server: "edge.example.com", want: "[JP] zouter"},
		{protocol: "vless", name: "Region: Tokyo", server: "edge.example.com", want: "Region: Tokyo"},
		{protocol: "vless", name: "Authority Lab", server: "edge.example.com", want: "Authority Lab"},
		{protocol: "vless", name: "Tokyo #1", server: "edge.example.com", want: "Tokyo #1"},
		{protocol: "vless", name: "John's Node", server: "edge.example.com", want: "John's Node"},
	} {
		if got := NormalizeProxyName(test.protocol, test.name, test.server); got != test.want {
			t.Errorf("NormalizeProxyName(%q, %q) = %q, want %q", test.protocol, test.name, got, test.want)
		}
	}
}

func TestNormalizeProxyNameFallsBackForSensitiveLabels(t *testing.T) {
	for _, test := range []struct {
		protocol string
		name     string
		server   string
		want     string
	}{
		{protocol: "vless", name: "vless://uuid:password@private.example:443", want: "vless-node"},
		{protocol: "shadowsocks", name: "1.2.3.4", want: "ss-node"},
		{protocol: "vmess", name: "private.example.com/path", want: "vmess-node"},
		{protocol: "vmess", name: `{"add":"private.example.com","id":"secret"}`, want: "vmess-node"},
		{protocol: "vless", name: "user:password", want: "vless-node"},
		{protocol: "vless", name: "private.example.com", server: "private.example.com", want: "vless-node"},
		{protocol: "vless", name: "backup 192.0.2.1", want: "vless-node"},
		{protocol: "vless", name: "route edge.example.com", want: "vless-node"},
		{protocol: "vless", name: "00000000-0000-0000-0000-000000000001", want: "vless-node"},
		{protocol: "vless", name: "vless-localhost", want: "vless-node"},
		{protocol: "vless", name: "vless%3A%2F%2Fsecret", want: "vless-node"},
		{protocol: "vless://uuid:password@secret.example:443", name: "", want: "proxy-node"},
		{protocol: "vless", name: "foo?x=secret", want: "vless-node"},
	} {
		if got := NormalizeProxyName(test.protocol, test.name, test.server); got != test.want {
			t.Errorf("NormalizeProxyName(%q, %q) = %q, want %q", test.protocol, test.name, got, test.want)
		}
	}
}

func TestNormalizePublicResultLabelRejectsHostsExceptFixedFallback(t *testing.T) {
	for _, value := range []string{
		"proxy.example.com",
		"edge.internal",
		"192.0.2.44",
		"provider proxy.example.com",
		"foo?x=secret",
		"foo&bar=secret",
	} {
		if got := NormalizePublicResultLabel(value, 256); got != "" {
			t.Errorf("host-shaped public label %q normalized to %q", value, got)
		}
	}
	for _, value := range []string{"Speedtest.net", "Tokyo", "Example ISP", "Region: Tokyo"} {
		if got := NormalizePublicResultLabel(value, 256); got != value {
			t.Errorf("safe public label %q normalized to %q", value, got)
		}
	}
}

func TestNormalizeClientMetadata(t *testing.T) {
	if got := NormalizeClientName("  aws-sg-01  "); got != "aws-sg-01" {
		t.Fatalf("normal Client name changed to %q", got)
	}
	for _, sensitive := range []string{
		"192.0.2.10",
		"edge.private.example",
		"../private/client.key",
		"vless://uuid:password@private.example:443/private/path",
		"0123456789abcdef0123456789abcdef",
	} {
		if got := NormalizeClientName(sensitive); got != "client-node" {
			t.Errorf("sensitive Client name %q normalized to %q", sensitive, got)
		}
	}

	labels := map[string]string{
		"region":       "cn-east",
		"provider":     "Example Telecom",
		" region ":     "must be removed",
		"bad.key":      "visible",
		"private_path": "../etc/smalux/client.key",
		"endpoint":     "192.0.2.20:443",
		"credential":   "vless://uuid:password@private.example:443",
		strings.Repeat("k", maxClientLabelKeyBytes+1): "too long",
	}
	want := map[string]string{"provider": "Example Telecom", "region": "cn-east"}
	if got := NormalizeClientLabels(labels); !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized Client labels = %#v, want %#v", got, want)
	}
	if labels["endpoint"] != "192.0.2.20:443" {
		t.Fatal("NormalizeClientLabels mutated its input")
	}
}
