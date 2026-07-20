package model

import (
	"encoding/json"
	"net/url"
	"strings"
)

const (
	// ResultErrorProxyInitialization 表示 sing-box 无法根据节点配置创建出站。
	ResultErrorProxyInitialization = "proxy initialization failed"
	// ResultErrorSpeedServerDiscovery 表示无法通过代理取得测速服务器列表。
	ResultErrorSpeedServerDiscovery = "speedtest server discovery failed"
	// ResultErrorNoSpeedServer 表示测速服务返回了空候选列表。
	ResultErrorNoSpeedServer = "no speedtest server available"
	// ResultErrorLatencyChecks 表示所有候选测速服务器的延迟检测均失败。
	ResultErrorLatencyChecks = "all speedtest latency checks failed"
	// ResultErrorDownload 表示只有下载阶段失败。
	ResultErrorDownload = "download test failed"
	// ResultErrorUpload 表示只有上传阶段失败。
	ResultErrorUpload = "upload test failed"
	// ResultErrorTransfer 表示下载与上传阶段均失败。
	ResultErrorTransfer = "download and upload tests failed"
	// ResultErrorProxyTest 是无法安全细分时使用的代理级通用错误。
	ResultErrorProxyTest = "proxy test failed"

	// TaskFailureTimeout、TaskFailureCanceled 和 TaskFailureClient 是允许持久化的任务级
	// 失败类别。客户端不得把底层网络或代理错误原文放入 Failure.Error。
	TaskFailureTimeout  = "task timed out"
	TaskFailureCanceled = "task canceled"
	TaskFailureClient   = "client task failed"

	// ResultProtocolUnknown is the safe wire/storage label for a protocol that is
	// not part of the importer/sing-box allowlist.
	ResultProtocolUnknown = "unknown"
)

// NormalizeResultProtocol applies the canonical protocol allowlist at trust
// boundaries. Importer aliases (socks5, https, hy2, etc.) are normalized before
// this point; accepting only canonical names here prevents arbitrary text from
// becoming a report grouping or an HTML/CSV field.
func NormalizeResultProtocol(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "shadowsocks", "vmess", "vless", "trojan", "hysteria", "hysteria2", "tuic", "socks", "http", "anytls", "ssh":
		return value
	default:
		return ResultProtocolUnknown
	}
}

// NormalizeResultError 只保留协议约定的固定结果错误类别。
//
// SpeedResult 来自远程 Client，属于不可信输入。即使新版 Client 已经在源头分类，Server
// 仍需在落库前执行相同白名单，防止旧版或被篡改 Client 把分享链接、节点地址、密码、
// UUID 或任意日志文本写入 SQLite、API、CSV、PNG 和 Telegram 图片。
func NormalizeResultError(value string) string {
	value = strings.TrimSpace(value)
	switch value {
	case "":
		return ""
	case ResultErrorProxyInitialization,
		ResultErrorSpeedServerDiscovery,
		ResultErrorNoSpeedServer,
		ResultErrorLatencyChecks,
		ResultErrorDownload,
		ResultErrorUpload,
		ResultErrorTransfer,
		ResultErrorProxyTest:
		return value
	default:
		return ResultErrorProxyTest
	}
}

// NormalizeTaskFailure 把客户端任务终态错误约束为固定类别。
// 标准 context 错误字符串仅用于兼容旧版 Client；其他任意文本统一折叠，避免 task 和
// task_targets 的 error 列成为节点信息的旁路持久化位置。
func NormalizeTaskFailure(value string) string {
	switch strings.TrimSpace(value) {
	case "context deadline exceeded", TaskFailureTimeout:
		return TaskFailureTimeout
	case "context canceled", TaskFailureCanceled:
		return TaskFailureCanceled
	case TaskFailureClient:
		return TaskFailureClient
	default:
		return TaskFailureClient
	}
}

// EraseAssignment 对运行结束的任务配置执行尽力而为的内存清理。
//
// Outbound 的 RawMessage 是可写字节切片，先逐字节覆盖再释放引用，可以缩短密码、UUID、
// 私钥等内容留在 Go heap 中的时间。Go 运行时、WebSocket 编码缓冲和内核缓冲可能曾经
// 复制这些字节，因此该函数不是对进程内存取证的绝对保证；传输仍必须使用受信任网络
// 或 WSS。清零整个 ProxySpec 还会释放服务器地址和用户提供的节点名称引用。
func EraseAssignment(assignment *Assignment) {
	if assignment == nil {
		return
	}
	for index := range assignment.Proxies {
		clear(assignment.Proxies[index].Outbound)
		assignment.Proxies[index] = ProxySpec{}
	}
	*assignment = Assignment{}
}

// containsEncodedDelimiter catches percent-encoded URI separators before a value
// can be rendered as if it were an ordinary label.
func containsEncodedDelimiter(value string) bool {
	for index := 0; index+2 < len(value); index++ {
		if value[index] != '%' {
			continue
		}
		first, second := value[index+1], value[index+2]
		isHex := func(current byte) bool {
			return (current >= '0' && current <= '9') || (current >= 'a' && current <= 'f') || (current >= 'A' && current <= 'F')
		}
		if !isHex(first) || !isHex(second) {
			continue
		}
		decoded, err := url.QueryUnescape(value[index : index+3])
		if err == nil && strings.ContainsAny(decoded, ":/@?#\\") {
			return true
		}
	}
	return false
}

// jsonShape catches both valid JSON objects/arrays and malformed values that visibly begin
// as JSON. The latter are rejected because an error/partial payload is still unsafe to display.
func jsonShape(value string) bool {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || (!strings.HasPrefix(trimmed, "{") && !strings.HasPrefix(trimmed, "[")) {
		return false
	}
	var decoded any
	if json.Unmarshal([]byte(trimmed), &decoded) == nil {
		return true
	}
	return strings.Contains(trimmed, ":") || strings.Contains(trimmed, "\"")
}

func looksLikeUUID(value string) bool {
	parts := strings.Split(strings.TrimSpace(value), "-")
	if len(parts) != 5 || len(parts[0]) != 8 || len(parts[1]) != 4 || len(parts[2]) != 4 || len(parts[3]) != 4 || len(parts[4]) != 12 {
		return false
	}
	for _, part := range parts {
		for _, current := range part {
			if (current < '0' || current > '9') && (current < 'a' || current > 'f') && (current < 'A' || current > 'F') {
				return false
			}
		}
	}
	return true
}

func looksLikeEncodedToken(value string) bool {
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
	return hasLetter && (hasDigit || len([]rune(value)) >= 32)
}
