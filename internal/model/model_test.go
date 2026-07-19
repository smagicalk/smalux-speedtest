package model

import "testing"

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
