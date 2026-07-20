package serverapp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"smalux-speedtest/internal/model"
)

const anonymousSpeedServerName = "Speedtest.net"

// normalizeResultMetadata constrains public Speedtest.net metadata supplied by a
// remote Client. These fields are useful in reports, but they are not trusted: a
// compromised Client has already received the outbound and could try to echo it
// through a nominal server name, sponsor, country, host or ID.
func normalizeResultMetadata(result *model.SpeedResult, proxy model.ProxySpec) {
	// IDs are used only for stable row identity. Hashing preserves equality without
	// storing an attacker-controlled token or a real Speedtest server identifier.
	result.SpeedServerID = opaqueSpeedServerID(result.SpeedServerID)
	result.SpeedServerName = safePublicResultLabel(result.SpeedServerName, maxResultNameRunes, proxy)
	if result.SpeedServerName == "" {
		result.SpeedServerName = anonymousSpeedServerName
	}
	// Host is not displayed by the dashboard or report and may contain an IP or URL.
	// Discarding it removes an unnecessary persistence surface.
	result.SpeedServerHost = ""
	result.Country = safePublicResultLabel(result.Country, maxResultCountryRunes, proxy)
	result.Sponsor = safePublicResultLabel(result.Sponsor, maxResultSponsorRunes, proxy)
}

func opaqueSpeedServerID(value string) string {
	if value == "" {
		return "server-unknown"
	}
	digest := sha256.Sum256([]byte(value))
	return "server-" + hex.EncodeToString(digest[:8])
}

// safePublicResultLabel retains ordinary human-readable location/provider text and
// rejects values shaped like an endpoint, path, share URI, JSON object or opaque
// credential. It also rejects every non-empty string value found in the assigned
// outbound, so even a one-character username/password cannot pass through a
// different result field. This can intentionally discard ambiguous public metadata
// for unusually short credentials; avoiding credential persistence takes priority.
func safePublicResultLabel(value string, limit int, proxy model.ProxySpec) string {
	value = model.NormalizePublicResultLabel(value, limit)
	if value == "" || containsOutboundValue(value, proxy) {
		return ""
	}
	return value
}

func containsOutboundValue(value string, proxy model.ProxySpec) bool {
	lower := strings.ToLower(value)
	if server := strings.ToLower(strings.TrimSpace(proxy.Server)); len(server) >= 3 && strings.Contains(lower, server) {
		return true
	}
	var outbound any
	if err := json.Unmarshal(proxy.Outbound, &outbound); err != nil {
		// A valid Assignment was produced by the importer, but fail closed if future
		// code constructs one differently.
		return true
	}
	return containsJSONScalar(lower, outbound)
}

func containsJSONScalar(value string, current any) bool {
	switch typed := current.(type) {
	case map[string]any:
		for _, nested := range typed {
			if containsJSONScalar(value, nested) {
				return true
			}
		}
	case []any:
		for _, nested := range typed {
			if containsJSONScalar(value, nested) {
				return true
			}
		}
	case string:
		candidate := strings.ToLower(strings.TrimSpace(typed))
		if candidate != "" && strings.Contains(value, candidate) {
			return true
		}
	}
	return false
}
