// Package logsafe provides log sanitization for structured error fields.
//
// Operational logs must preserve diagnostic root causes (such as network timeouts,
// connection refused, HTTP status codes, and configuration errors) while strictly
// excluding sensitive proxy share links, credentials, tokens, query parameters,
// and local user paths.
package logsafe

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	// proxyURIRegex matches proxy share link URIs and redacts them completely.
	proxyURIRegex = regexp.MustCompile(`(?i)\b(?:vless|vmess|ss|trojan|hysteria2|hy2|hysteria|tuic|anytls|socks5?|ssh)://[^\s"'<>\\]+`)

	// telegramBotTokenURLRegex matches Telegram Bot tokens in URL paths (e.g. /bot<token>/).
	telegramBotTokenURLRegex = regexp.MustCompile(`(?i)(/bot|bot)\d{5,14}:[a-zA-Z0-9_-]{10,}`)

	// telegramBotTokenRegex matches standalone Telegram Bot tokens (e.g. 123456789:AAH... or 555555:secret...).
	telegramBotTokenRegex = regexp.MustCompile(`\b\d{5,14}:[a-zA-Z0-9_-]{10,}\b`)

	// bearerTokenRegex matches Bearer authorization headers and tokens.
	bearerTokenRegex = regexp.MustCompile(`(?i)\b(Bearer\s+)[A-Za-z0-9_\.\-]+`)

	// urlCredentialRegex matches embedded basic auth in URLs (e.g. http://user:pass@host).
	urlCredentialRegex = regexp.MustCompile(`://[^/\s:@]+:[^/\s:@]+@`)

	// queryTokenRegex matches sensitive query parameter values such as token, password, secret, key, etc.
	queryTokenRegex = regexp.MustCompile(`(?i)([?&](?:token|password|passwd|secret|key|auth|credential|api_key|access_token)=)[^& \t\r\n"'>]+`)

	// envSecretRegex matches environment variable values for known sensitive keys.
	envSecretRegex = regexp.MustCompile(`(?i)\b(SMALUX_[A-Z_]*(?:PASSWORD|TOKEN|KEY)=)[^\s"'<>]+`)

	// jsonSecretRegex matches JSON string fields with sensitive keys.
	jsonSecretRegex = regexp.MustCompile(`(?i)"(password|token|secret|admin_password)"\s*:\s*"[^"]*"`)

	// dbPathRegex strips directory prefixes from sensitive SQLite and bot-key paths while preserving filename.
	dbPathRegex = regexp.MustCompile(`(?i)(?:[a-zA-Z]:[\\/][^\s"'<>\(\):]+[\\/]|/(?:[^\s"'<>\(\):]+/)+)([^\s"'<>\(\/\\]+\.(?:db|bot-key)(?:[^\s"'<>\(\):]*)?)`)

	// userHomeWinRegex strips Windows user home directories (e.g. C:\Users\username\...).
	userHomeWinRegex = regexp.MustCompile(`(?i)[a-zA-Z]:\\Users\\[^\s\\/:"'<>]+`)

	// userHomeUnixRegex strips Unix user home directories (e.g. /home/username/... or /Users/username/...).
	userHomeUnixRegex = regexp.MustCompile(`/(?:home|Users)/[^\s/:"'<>]+`)
)

// Sanitize removes sensitive credentials, proxy share links, tokens, passwords, and local user paths
// from an error message while preserving the root failure cause, status codes, socket errors, and diagnostics.
func Sanitize(msg string) string {
	if msg == "" {
		return ""
	}

	msg = proxyURIRegex.ReplaceAllString(msg, "[PROXY_URI_REDACTED]")
	msg = telegramBotTokenURLRegex.ReplaceAllString(msg, "${1}[REDACTED]")
	msg = telegramBotTokenRegex.ReplaceAllString(msg, "[REDACTED_BOT_TOKEN]")
	msg = bearerTokenRegex.ReplaceAllString(msg, "${1}[REDACTED]")
	msg = urlCredentialRegex.ReplaceAllString(msg, "://[REDACTED]@")
	msg = queryTokenRegex.ReplaceAllString(msg, "${1}[REDACTED]")
	msg = envSecretRegex.ReplaceAllString(msg, "${1}[REDACTED]")
	msg = jsonSecretRegex.ReplaceAllString(msg, `"$1":"[REDACTED]"`)
	msg = dbPathRegex.ReplaceAllString(msg, "[PATH]/$1")
	msg = userHomeWinRegex.ReplaceAllString(msg, `~`)
	msg = userHomeUnixRegex.ReplaceAllString(msg, `~`)

	return strings.TrimSpace(msg)
}

// Error returns the sanitized error message, or "" if err is nil.
func Error(err error) string {
	if err == nil {
		return ""
	}
	return Sanitize(err.Error())
}

// ErrorType returns the concrete Go error type and the sanitized diagnostic message.
// It redacts sensitive credentials, tokens, and proxy links while ensuring the underlying
// error message remains visible and useful for troubleshooting.
func ErrorType(err error) string {
	if err == nil {
		return "<nil>"
	}
	sanitized := Sanitize(err.Error())
	if sanitized == "" {
		return fmt.Sprintf("%T", err)
	}
	return fmt.Sprintf("%T: %s", err, sanitized)
}
