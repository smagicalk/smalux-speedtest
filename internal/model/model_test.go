package model

import "testing"

// TestMaskAddress 覆盖最常见的 IPv4 和多标签域名脱敏规则，并同时确认端口被保留。
// 该测试防止历史结果或导出内容在重构后意外暴露完整代理地址。
func TestMaskAddress(t *testing.T) {
	tests := map[string]string{
		MaskAddress("192.0.2.10", 443):      "192.0.*.*:443",
		MaskAddress("edge.example.com", 80): "*.example.com:80",
	}
	for actual, expected := range tests {
		if actual != expected {
			t.Fatalf("got %q, want %q", actual, expected)
		}
	}
}
