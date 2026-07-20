package model

import "testing"

// TestMaskAddress 覆盖 IPv4、IPv6 和域名，并确认结果完全不包含主机信息。
// 该测试防止历史结果或导出内容在重构后重新暴露地址前缀或域名后缀。
func TestMaskAddress(t *testing.T) {
	tests := map[string]string{
		"192.0.2.10":       MaskAddress("192.0.2.10", 443),
		"2001:db8::1234":   MaskAddress("2001:db8::1234", 443),
		"edge.example.com": MaskAddress("edge.example.com", 443),
	}
	for source, actual := range tests {
		if actual != "[redacted]:443" {
			t.Fatalf("MaskAddress(%q) = %q", source, actual)
		}
	}
}
