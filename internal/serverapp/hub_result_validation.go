package serverapp

import (
	"math"
	"strings"
	"time"
	"unicode"

	"smalux-speedtest/internal/model"
)

const (
	maxResultNameRunes       = 256
	maxResultHostRunes       = 256
	maxResultCountryRunes    = 64
	maxResultSponsorRunes    = 256
	maxResultDelayMS         = 3_600_000
	maxResultSpeedBPS        = 10_000_000_000_000
	defaultResultDurationMax = 10 * time.Minute
)

// resultProxyIdentity 是允许持久化的服务端代理快照，不包含 Outbound 凭据。
type resultProxyIdentity struct {
	name          string
	protocol      string
	maskedAddress string
}

func resultIdentityFromProxy(proxy model.ProxySpec) resultProxyIdentity {
	protocol := model.NormalizeResultProtocol(proxy.Protocol)
	return resultProxyIdentity{
		// Assignment names may come from an importer, an API caller or an older
		// integration. Re-apply the shared rule before the value enters task state;
		// this also protects progress SSE, which is emitted before SaveResult.
		name:          boundedResultText(model.NormalizeProxyName(protocol, proxy.Name, proxy.Server), maxResultNameRunes),
		protocol:      protocol,
		maskedAddress: boundedResultText(model.MaskAddress(proxy.Server, proxy.Port), maxResultHostRunes),
	}
}

// validateResult 绑定 Client 结果与 Assignment 中的代理身份、约束可变文本和数值，并
// 在落库前检查每个目标的唯一结果上限。调用方必须持有 runtimeTask.transition。
func (t *runtimeTask) validateResult(clientID string, result model.SpeedResult) (model.SpeedResult, string, bool) {
	if len(result.ProxyID) > 128 {
		return model.SpeedResult{}, "", false
	}
	identity, exists := t.resultProxies[result.ProxyID]
	if !exists || t.resultLimit < 1 {
		return model.SpeedResult{}, "", false
	}
	result.TaskID = t.assignment.TaskID
	result.ClientID = clientID
	result.ProxyName = identity.name
	result.Protocol = identity.protocol
	result.MaskedAddress = identity.maskedAddress
	var assignedProxy model.ProxySpec
	for _, proxy := range t.assignment.Proxies {
		if proxy.ID == result.ProxyID {
			assignedProxy = proxy
			break
		}
	}
	if assignedProxy.ID == "" {
		return model.SpeedResult{}, "", false
	}
	normalizeResultMetadata(&result, assignedProxy)
	// Error 来自远程 Client。固定类别白名单比正则脱敏更可靠：任意旧版或恶意
	// Client 的原始错误都会折叠为通用文本，不能把节点配置带入持久化结果。
	result.Error = model.NormalizeResultError(result.Error)
	result.LatencyMS = boundedMetric(result.LatencyMS, maxResultDelayMS)
	result.JitterMS = boundedMetric(result.JitterMS, maxResultDelayMS)
	result.DownloadBPS = boundedMetric(result.DownloadBPS, maxResultSpeedBPS)
	result.UploadBPS = boundedMetric(result.UploadBPS, maxResultSpeedBPS)
	durationLimit := int64(defaultResultDurationMax / time.Millisecond)
	if t.assignment.TimeoutSeconds > 0 {
		durationLimit = int64(t.assignment.TimeoutSeconds) * 1000
	}
	if result.DurationMS < 0 {
		result.DurationMS = 0
	} else if result.DurationMS > durationLimit {
		result.DurationMS = durationLimit
	}
	// Client 时钟不属于持久化信任边界；服务端接收时间保证排序和格式稳定。
	result.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)

	key := result.ProxyID + "\x00" + result.SpeedServerID
	keys := t.resultKeys[clientID]
	if _, duplicate := keys[key]; !duplicate {
		counts := t.resultCounts[clientID]
		if len(keys) >= t.resultLimit || counts[result.ProxyID] >= t.resultLimitPerProxy {
			return model.SpeedResult{}, "", false
		}
	}
	return result, key, true
}

// recordResultKey 只在 SaveResult 成功后记账；数据库瞬时失败可以由 Client 重发。
func (t *runtimeTask) recordResultKey(clientID, key string) {
	keys := t.resultKeys[clientID]
	if keys == nil {
		keys = make(map[string]struct{})
		t.resultKeys[clientID] = keys
	}
	if _, exists := keys[key]; exists {
		return
	}
	keys[key] = struct{}{}
	separator := strings.IndexByte(key, 0)
	proxyID := key
	if separator >= 0 {
		proxyID = key[:separator]
	}
	counts := t.resultCounts[clientID]
	if counts == nil {
		counts = make(map[string]int)
		t.resultCounts[clientID] = counts
	}
	counts[proxyID]++
}

func boundedResultText(value string, limit int) string {
	value = strings.TrimSpace(value)
	result := make([]rune, 0, min(len(value), limit))
	for _, current := range value {
		if unicode.IsControl(current) {
			continue
		}
		if len(result) == limit {
			break
		}
		result = append(result, current)
	}
	return string(result)
}

func boundedMetric(value, maximum float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0
	}
	if value > maximum {
		return maximum
	}
	return value
}
