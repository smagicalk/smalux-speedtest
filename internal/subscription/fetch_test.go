package subscription

import (
	"net/netip"
	"net/url"
	"testing"
)

// TestValidateURLBlocksUnsafeDestinations 覆盖非 HTTP 协议、IPv4/IPv6 回环和 URL
// userinfo，并以公网 HTTPS URL 确认静态规则不过度拒绝正常输入。
func TestValidateURLBlocksUnsafeDestinations(t *testing.T) {
	// 域名 DNS 结果校验位于 DialContext，不能由这个静态 URL 单元测试替代。
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

// TestPublicAddress 验证 SSRF 地址分类的核心边界：RFC1918 私网和 IPv4 链路本地必须
// 拒绝，明确公网地址必须允许，使地址策略不依赖 URL 解析实现。
func TestPublicAddress(t *testing.T) {
	if publicAddress(netip.MustParseAddr("10.0.0.1")) || publicAddress(netip.MustParseAddr("169.254.1.1")) {
		t.Fatal("private address accepted")
	}
	if !publicAddress(netip.MustParseAddr("1.1.1.1")) {
		t.Fatal("public address rejected")
	}
}
