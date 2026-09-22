package logsafe

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestErrorTypeDoesNotIncludeMessage(t *testing.T) {
	const secret = "vless://uuid:password@192.0.2.10/private/path"
	value := ErrorType(errors.New(secret))
	if strings.Contains(value, secret) || strings.Contains(value, "192.0.2.10") {
		t.Fatalf("error type exposed message: %q", value)
	}
	if !strings.Contains(value, "[PROXY_URI_REDACTED]") {
		t.Fatalf("expected redacted proxy link, got: %q", value)
	}
}

func TestSanitizeProxyProtocols(t *testing.T) {
	proxies := []string{
		"ss://YWVzLTEyOC1nY206cGFzc3dvcmQ=@192.0.2.1:8388#Node",
		"vmess://eyJhZGQiOiIxOTIuMC4yLjEiLCJwb3J0IjoiNDQzIn0=",
		"vless://uuid-1234@example.com:443?security=reality&pbk=secretkey",
		"trojan://my-password@trojan.example.com:443",
		"hysteria2://pass123@hy2.example.com:443",
		"hy2://pass123@hy2.example.com:443",
		"tuic://uuid:pass@tuic.example.com:443",
		"anytls://pass@anytls.example.com:443",
		"socks5://user:pass@127.0.0.1:1080",
	}

	for _, proxy := range proxies {
		err := fmt.Errorf("failed to dial %s: network unreachable", proxy)
		result := ErrorType(err)
		if strings.Contains(result, proxy) {
			t.Errorf("proxy URI exposed in log: %q", result)
		}
		if !strings.Contains(result, "network unreachable") {
			t.Errorf("expected diagnostic error 'network unreachable' to be preserved, got: %q", result)
		}
	}
}

func TestSanitizeTelegramBotToken(t *testing.T) {
	const token = "123456789:ABCdefGhIJKlmNoPQRsTUVwxyZ123456789"
	errInURL := fmt.Errorf("Get \"https://api.telegram.org/bot%s/getUpdates\": context deadline exceeded", token)
	resultURL := ErrorType(errInURL)
	if strings.Contains(resultURL, token) {
		t.Fatalf("bot token exposed in URL: %q", resultURL)
	}
	if !strings.Contains(resultURL, "context deadline exceeded") {
		t.Fatalf("context deadline exceeded was stripped: %q", resultURL)
	}

	errStandalone := fmt.Errorf("telegram bot token %s is invalid", token)
	resultStandalone := ErrorType(errStandalone)
	if strings.Contains(resultStandalone, token) {
		t.Fatalf("standalone bot token exposed: %q", resultStandalone)
	}
}

func TestSanitizeTokensAndCredentials(t *testing.T) {
	const bearerToken = "secret_client_token_abcdef123456"
	errBearer := fmt.Errorf("handshake failed: Bearer %s rejected", bearerToken)
	resultBearer := ErrorType(errBearer)
	if strings.Contains(resultBearer, bearerToken) {
		t.Fatalf("bearer token exposed: %q", resultBearer)
	}
	if !strings.Contains(resultBearer, "handshake failed") {
		t.Fatalf("handshake failed error stripped: %q", resultBearer)
	}

	const queryToken = "my_private_sub_token"
	errQuery := fmt.Errorf("fetch https://sub.example.com/sub?token=%s&id=1: HTTP 500", queryToken)
	resultQuery := ErrorType(errQuery)
	if strings.Contains(resultQuery, queryToken) {
		t.Fatalf("query token exposed: %q", resultQuery)
	}
	if !strings.Contains(resultQuery, "token=[REDACTED]") || !strings.Contains(resultQuery, "HTTP 500") {
		t.Fatalf("query token redaction failed or HTTP 500 stripped: %q", resultQuery)
	}

	errBasic := fmt.Errorf("connect to http://admin:super_secret_pw@example.com/api failed")
	resultBasic := ErrorType(errBasic)
	if strings.Contains(resultBasic, "super_secret_pw") {
		t.Fatalf("basic auth password exposed: %q", resultBasic)
	}
}

func TestSanitizePathsAndSecrets(t *testing.T) {
	errDB := fmt.Errorf("open C:\\Users\\Administrator\\AppData\\smalux-speedtest.db: permission denied")
	resultDB := ErrorType(errDB)
	if strings.Contains(resultDB, "Administrator") {
		t.Fatalf("Windows username exposed in path: %q", resultDB)
	}
	if !strings.Contains(resultDB, "smalux-speedtest.db") || !strings.Contains(resultDB, "permission denied") {
		t.Fatalf("filename or diagnostic error stripped: %q", resultDB)
	}

	errUnix := fmt.Errorf("open /home/secretuser/data/smalux-speedtest.db: permission denied")
	resultUnix := ErrorType(errUnix)
	if strings.Contains(resultUnix, "secretuser") {
		t.Fatalf("Unix username exposed in path: %q", resultUnix)
	}

	errEnv := fmt.Errorf("invalid env: SMALUX_ADMIN_PASSWORD=my_admin_pass")
	resultEnv := ErrorType(errEnv)
	if strings.Contains(resultEnv, "my_admin_pass") {
		t.Fatalf("env password exposed: %q", resultEnv)
	}
}

func TestPreservesDiagnosticErrors(t *testing.T) {
	testCases := []string{
		"dial tcp 127.0.0.1:8080: connect: connection refused",
		"listen tcp 127.0.0.1:8080: bind: address already in use",
		"websocket dial returned HTTP 401: Unauthorized",
		"context deadline exceeded",
		"database is locked",
		"admin password required for initial start",
		"server URL and token are required",
		"telegram sendMessage failed (http=400, code=400): Bad Request: chat not found",
	}

	for _, tc := range testCases {
		err := errors.New(tc)
		result := ErrorType(err)
		if !strings.Contains(result, tc) {
			t.Errorf("expected diagnostic error %q to be preserved, got: %q", tc, result)
		}
	}
}

func TestErrorTypeNil(t *testing.T) {
	if actual := ErrorType(nil); actual != "<nil>" {
		t.Fatalf("ErrorType(nil) = %q, want <nil>", actual)
	}
	if actual := Error(nil); actual != "" {
		t.Fatalf("Error(nil) = %q, want empty string", actual)
	}
}
