package model

import (
	"strings"
)

const maxProxyNameRunes = 256

// NormalizeProxyName returns a display name that is safe to persist and expose.
//
// Subscription authors control URI fragments and VMess "ps" values. Those fields are
// commonly treated as harmless labels, but they can also contain a complete share URI,
// a host/path, JSON or credentials. The fallback deliberately contains only the protocol
// family and a fixed suffix, so a result name can never become a second address channel.
// The server and Store call this function again because a remote Client or a future caller
// may construct ProxySpec/SpeedResult without going through importer.
func NormalizeProxyName(protocol, name, server string) string {
	fallback := proxyNameFallback(protocol)
	trimmed, hadControl := cleanProxyName(name)
	if trimmed == "" || hadControl || sensitiveProxyName(trimmed, server) {
		return fallback
	}
	return trimmed
}

// NormalizePublicResultLabel keeps ordinary public metadata while rejecting syntax
// that can carry an endpoint, path, JSON payload, URI, bare host/domain or token.
// The fixed Speedtest.net name is an intentional exception: it is a public product
// label used by the report fallback, not an endpoint supplied by the Client.
// An empty return means the value must be omitted by the caller.
func NormalizePublicResultLabel(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	trimmed, hadControl := cleanProxyName(value)
	if trimmed == "" || hadControl || sensitivePublicResultLabel(trimmed) {
		return ""
	}
	return limitPrivacyRunes(trimmed, limit)
}

func sensitivePublicResultLabel(value string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	if lower == "" || strings.Contains(lower, "://") || strings.ContainsAny(value, `/\\@="{}?&`) {
		return true
	}
	if containsEncodedDelimiter(lower) || jsonShape(value) {
		return true
	}
	// A bare endpoint or domain is not useful public metadata. Keep the one
	// fixed report label that is deliberately known to be safe.
	if !strings.EqualFold(strings.TrimSpace(value), "Speedtest.net") && looksLikeIPOrHost(value) {
		return true
	}
	// A colon without whitespace is normally host:port or user:secret. Keep
	// prose such as "Region: Tokyo" usable.
	if strings.Contains(value, ":") && !strings.ContainsAny(value, " \t\r\n") {
		return true
	}
	for _, keyword := range []string{"password", "passwd", "token", "secret", "credential", "private_key", "privatekey", "api_key", "apikey", "uuid"} {
		if strings.Contains(lower, keyword) {
			return true
		}
	}
	return looksLikePublicToken(value)
}

func looksLikePublicToken(value string) bool {
	value = strings.TrimSpace(value)
	if len([]rune(value)) < 24 || strings.ContainsAny(value, " \t") {
		return false
	}
	hasLetter, hasDigit := false, false
	for _, current := range value {
		switch {
		case current >= 'a' && current <= 'z', current >= 'A' && current <= 'Z':
			hasLetter = true
		case current >= '0' && current <= '9':
			hasDigit = true
		case strings.ContainsRune("-_.+/=", current):
		default:
			return false
		}
	}
	return hasLetter && hasDigit
}

// proxyNameFallback normalizes protocol text before placing it in a fallback. Keep the
// historical short label "ss-node" for Shadowsocks so existing reports remain recognizable.
func proxyNameFallback(protocol string) string {
	protocol = strings.ToLower(strings.TrimSpace(protocol))
	// Protocol is not intrinsically trusted: legacy rows and direct integrations
	// may contain a complete URI or arbitrary text. Never filter-and-concatenate
	// that value, because the remaining characters can still spell a hostname or
	// credential. Map only the importer/storage allowlist and use one fixed label
	// for everything else.
	switch protocol {
	case "ss", "shadowsocks":
		return "ss-node"
	case "vmess", "vless", "trojan", "hysteria", "hysteria2", "tuic", "socks", "http", "anytls", "ssh":
		return protocol + "-node"
	case "hy2":
		return "hysteria2-node"
	case "socks5":
		return "socks-node"
	case "https":
		return "http-node"
	default:
		return "proxy-node"
	}
}

// sensitiveProxyName intentionally favors privacy over retaining an ambiguous label. It
// rejects URI/JSON/path syntax, host and IP-shaped values, encoded URI delimiters, obvious
// credential keys, and opaque token-like strings. A normal human label such as "Tokyo 01"
// does not match these checks and is retained unchanged.
func sensitiveProxyName(value, server string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	if lower == "" {
		return true
	}
	if strings.Contains(lower, "://") || strings.ContainsAny(value, `/\\@="{}?&`) {
		return true
	}
	if containsEncodedDelimiter(lower) {
		return true
	}
	if jsonShape(value) || looksLikeIPOrHost(value) || matchesProxyServer(value, server) {
		return true
	}
	for _, keyword := range []string{"password", "passwd", "token", "secret", "credential", "private_key", "privatekey", "api_key", "apikey", "uuid"} {
		if strings.Contains(lower, keyword) {
			return true
		}
	}
	// A colon without whitespace is the common user:password/uuid:token form. A colon
	// surrounded by ordinary words (for example "Region: Tokyo") remains usable.
	if strings.Contains(value, ":") && !strings.ContainsAny(value, " \t\r\n") {
		return true
	}
	if looksLikeUUID(value) || looksLikeEncodedToken(value) {
		return true
	}
	return false
}
