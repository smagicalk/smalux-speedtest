package model

import (
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"unicode"
)

const (
	maxClientNameRunes       = 96
	maxClientLabelKeyBytes   = 32
	maxClientLabelValueRunes = 96
	maxClientLabels          = 32
	anonymousClientName      = "client-node"
)

// NormalizeClientName preserves ordinary administrator-provided Client labels while
// preventing the name column from becoming another place to store an endpoint,
// filesystem path, share URI or credential. The same rule is applied when creating a
// Client, reading it, and migrating rows that older Client Hello messages could replace.
func NormalizeClientName(value string) string {
	trimmed, hadControl := cleanProxyName(value)
	if trimmed == "" || hadControl || sensitiveProxyName(trimmed, "") {
		return anonymousClientName
	}
	return limitPrivacyRunes(trimmed, maxClientNameRunes)
}

// NormalizeClientLabels returns a new map containing only bounded, display-oriented
// administrator metadata. Keys use a deliberately small ASCII grammar; values share the
// endpoint/path/credential detection used for names. Sorting before the 32-label limit
// makes normalization deterministic even though Go map iteration order is unspecified.
func NormalizeClientLabels(labels map[string]string) map[string]string {
	if len(labels) == 0 {
		return nil
	}
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	normalized := make(map[string]string, min(len(keys), maxClientLabels))
	for _, originalKey := range keys {
		key := strings.TrimSpace(originalKey)
		if key != originalKey || !validClientLabelKey(key) {
			continue
		}
		value, ok := normalizeClientLabelValue(labels[originalKey])
		if !ok {
			continue
		}
		if _, exists := normalized[key]; exists {
			continue
		}
		normalized[key] = value
		if len(normalized) == maxClientLabels {
			break
		}
	}
	if len(normalized) == 0 {
		return nil
	}
	return normalized
}

func validClientLabelKey(value string) bool {
	if value == "" || len(value) > maxClientLabelKeyBytes {
		return false
	}
	for _, current := range value {
		if (current >= 'a' && current <= 'z') || (current >= 'A' && current <= 'Z') ||
			(current >= '0' && current <= '9') || current == '_' || current == '-' {
			continue
		}
		return false
	}
	return true
}

func normalizeClientLabelValue(value string) (string, bool) {
	trimmed, hadControl := cleanProxyName(value)
	if trimmed == "" || hadControl || sensitiveProxyName(trimmed, "") {
		return "", false
	}
	return limitPrivacyRunes(trimmed, maxClientLabelValueRunes), true
}

func limitPrivacyRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) > limit {
		runes = runes[:limit]
	}
	return string(runes)
}

// cleanProxyName removes surrounding whitespace and bounds a value before it reaches
// result rows or reports. Control characters are reported separately rather than silently
// removed: a label containing a newline or escape sequence is not a trustworthy display name.
func cleanProxyName(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", false
	}
	runes := make([]rune, 0, maxProxyNameRunes)
	hadControl := false
	for _, current := range value {
		if unicode.IsControl(current) {
			hadControl = true
			continue
		}
		if len(runes) == maxProxyNameRunes {
			break
		}
		runes = append(runes, current)
	}
	return string(runes), hadControl
}

func looksLikeIPOrHost(value string) bool {
	if looksLikeIPOrHostToken(value) {
		return true
	}
	// Also reject an endpoint embedded in otherwise harmless-looking prose, such as
	// "backup 192.0.2.1" or "route edge.example.com".
	for _, token := range strings.FieldsFunc(value, func(current rune) bool {
		return unicode.IsSpace(current) || strings.ContainsRune(",;()<>|", current)
	}) {
		if token != value && looksLikeIPOrHostToken(strings.Trim(token, ".[]")) {
			return true
		}
	}
	return false
}

func looksLikeIPOrHostToken(value string) bool {
	trimmed := strings.TrimSpace(value)
	if strings.EqualFold(trimmed, "localhost") || strings.HasSuffix(strings.ToLower(trimmed), "-localhost") {
		return true
	}
	if addr, err := netip.ParseAddr(strings.Trim(trimmed, "[]")); err == nil && addr.IsValid() {
		return true
	}
	// Parse host:port and bracketed IPv6 through net/url. A plain domain is handled below.
	if parsed, err := url.Parse("//" + trimmed); err == nil {
		host := parsed.Hostname()
		if host != "" {
			if addr, addrErr := netip.ParseAddr(host); addrErr == nil && addr.IsValid() {
				return true
			}
			if strings.Contains(host, ".") && validHostname(host) {
				return true
			}
		}
	}
	if strings.ContainsAny(trimmed, " \t") || !strings.Contains(trimmed, ".") {
		return false
	}
	return validHostname(trimmed)
}

func validHostname(value string) bool {
	value = strings.TrimSuffix(strings.ToLower(value), ".")
	if value == "" || len(value) > 253 {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, current := range label {
			if !unicode.IsLetter(current) && !unicode.IsDigit(current) && current != '-' {
				return false
			}
		}
	}
	return true
}

func matchesProxyServer(value, server string) bool {
	server = strings.TrimSpace(strings.ToLower(server))
	if server == "" {
		return false
	}
	host := server
	if parsed, err := url.Parse("//" + server); err == nil && parsed.Hostname() != "" {
		host = strings.ToLower(parsed.Hostname())
	}
	value = strings.ToLower(strings.TrimSpace(value))
	return value == host || strings.Contains(value, host)
}
