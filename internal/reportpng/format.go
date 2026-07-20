package reportpng

import (
	"fmt"
	"math"
	"sort"
	"time"

	"smalux-speedtest/internal/model"
)

// visibleResults 复制受限数量的结果后排序，既不改变调用方切片，也不因超大输入分配
// 与总结果数等量的临时内存。
func visibleResults(results []model.SpeedResult) []model.SpeedResult {
	count := len(results)
	if count > MaxRows {
		count = MaxRows
	}
	visible := append([]model.SpeedResult(nil), results[:count]...)
	sort.SliceStable(visible, func(i, j int) bool {
		left, right := visible[i], visible[j]
		if left.ProxyName != right.ProxyName {
			return left.ProxyName < right.ProxyName
		}
		if left.ProxyID != right.ProxyID {
			return left.ProxyID < right.ProxyID
		}
		if left.ClientName != right.ClientName {
			return left.ClientName < right.ClientName
		}
		if left.ClientID != right.ClientID {
			return left.ClientID < right.ClientID
		}
		return finiteValue(left.LatencyMS) < finiteValue(right.LatencyMS)
	})
	return visible
}

func maxMetric(results []model.SpeedResult, metric func(model.SpeedResult) float64) float64 {
	maximum := 1.0
	for _, result := range results {
		value := metric(result)
		if isFinitePositive(value) && value > maximum {
			maximum = value
		}
	}
	return maximum
}

func ratio(value, maximum float64) float64 {
	if !isFinitePositive(value) || !isFinitePositive(maximum) {
		return 0
	}
	value /= maximum
	if value > 1 {
		return 1
	}
	return value
}

func finiteValue(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return math.MaxFloat64
	}
	return value
}

func isFinitePositive(value float64) bool {
	return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func collectRunes(value string, limit int) ([]rune, bool) {
	runes := make([]rune, 0, min(len(value), limit))
	for _, current := range value {
		if len(runes) == limit {
			return runes, true
		}
		runes = append(runes, current)
	}
	return runes, false
}

func limitRunes(value string, limit int) string {
	runes, limited := collectRunes(value, limit)
	if limited {
		return string(runes) + "..."
	}
	return string(runes)
}

func formatDelay(value float64) string {
	if !isFinitePositive(value) {
		return "-"
	}
	return fmt.Sprintf("%.1f ms", value)
}

func formatSpeed(value float64) string {
	if !isFinitePositive(value) {
		return "-"
	}
	if value >= 1_000_000_000 {
		return fmt.Sprintf("%.2f Gbps", value/1_000_000_000)
	}
	return fmt.Sprintf("%.2f Mbps", value/1_000_000)
}

func formatTaskTime(value string) string {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		// CreatedAt normally comes from Store, but reports can also be rendered by
		// tests or future callers. Never turn an invalid timestamp into visible
		// text because a restored or manually edited database may contain a URL,
		// credential, address, or local path in this column.
		return "-"
	}
	return parsed.UTC().Format("2006-01-02 15:04:05 UTC")
}

func shortID(value string) string {
	if value == "" {
		return "-"
	}
	return limitRunes(value, 12)
}
