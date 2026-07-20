package importer

import (
	"strconv"
	"strings"

	"smalux-speedtest/internal/model"
)

// supportedSchemes 是采用标准 URI 结构、可进入通用解析流程的协议白名单。
// ss/vmess 在 ParseLink 中走专用解析器；ssr 保留在识别集合附近，但会明确报告
// sing-box 1.13 已移除支持，避免把“已识别”误认为“可测速”。
var supportedSchemes = map[string]bool{
	"ss": true, "ssr": true, "vmess": true, "vless": true, "trojan": true,
	"hysteria": true, "hysteria2": true, "hy2": true, "tuic": true,
	"socks": true, "socks5": true, "http": true, "https": true,
	"anytls": true, "ssh": true,
}

// Parse 解析一份订阅正文，返回成功代理和逐行错误。
//
// 输入可以是单条链接、多行链接或整体 Base64 编码的多行订阅。单行失败不会阻止其他
// 行继续导入；空行和以 # 开头的注释行被忽略。重复项按“协议、服务器、端口、名称”
// 去重，这一键保留同地址但不同名称的节点，也不会把不同协议的同一端口合并。
func Parse(content string) model.ImportResult {
	// 去除 UTF-8 BOM，兼容由 Windows/文本编辑器生成的订阅文件。
	content = strings.TrimSpace(strings.TrimPrefix(content, "\ufeff"))
	// 只有正文自身不含 URI 且解码结果看起来包含 URI 时才视为整体 Base64，避免把
	// vmess:// 后面的局部 Base64 或普通分享链接误解码为第二层订阅。
	if decoded, ok := decodeSubscription(content); ok {
		content = decoded
	}

	var result model.ImportResult
	seen := make(map[string]bool)
	// 先统一 CRLF；保留 Split 后的原始下标，使错误 Line 对应用户看到的订阅行号。
	for index, line := range strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		proxy, err := ParseLink(line)
		if err != nil {
			// API 只返回协议提示和固定错误类别。截断仍可能暴露短链接的完整密码，
			// 因此不能把任何原始片段带出 importer。
			result.Errors = append(result.Errors, model.ImportError{Line: index + 1, Input: redactInput(line), Error: safeImportError(err)})
			continue
		}
		key := proxy.Protocol + "|" + proxy.Server + "|" + strconv.Itoa(int(proxy.Port)) + "|" + proxy.Name
		if seen[key] {
			continue
		}
		seen[key] = true
		result.Proxies = append(result.Proxies, proxy)
	}
	return result
}

// decodeSubscription 尝试识别“整个订阅正文被 Base64 包裹”的格式。
//
// 先检查原文不含 ://，再移除所有空白后尝试四种 Base64 变体，最后要求解码文本
// 至少包含一个 URI 标记。这些启发式条件可兼容邮件式换行的订阅，同时降低把普通
// 文本或单条 VMess 负载误判为外层订阅的概率。返回 false 时调用方继续按原文逐行解析。
func decodeSubscription(content string) (string, bool) {
	if strings.Contains(content, "://") {
		return "", false
	}
	decoded, err := decodeBase64(strings.Join(strings.Fields(content), ""))
	if err != nil || !strings.Contains(decoded, "://") {
		return "", false
	}
	return decoded, true
}

// redactInput 只保留格式合法且长度受限的 URI scheme。
// 分享链接的 userinfo、authority、query、fragment 和 Base64 载荷均可能包含凭据，因此
// 无论原文长短都不能回显。无法可靠识别 scheme 时只返回统一占位符。
func redactInput(value string) string {
	separator := strings.Index(value, "://")
	if separator < 1 || separator > 20 {
		return "[redacted]"
	}
	scheme := strings.ToLower(value[:separator])
	for _, current := range scheme {
		if (current < 'a' || current > 'z') && (current < '0' || current > '9') && current != '+' && current != '-' && current != '.' {
			return "[redacted]"
		}
	}
	return scheme + "://[redacted]"
}

// safeImportError 将解析器和标准库错误折叠成不含输入内容的类别。
// net/url 和 JSON/Base64 解析错误在部分情况下会引用原始字符串或其片段，不能直接
// 放入 HTTP/Telegram 响应，更不能被上层日志记录。
func safeImportError(err error) string {
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "unsupported protocol"), strings.Contains(message, "removed by sing-box"):
		return "proxy protocol is not supported"
	case strings.Contains(message, "server"), strings.Contains(message, "port"):
		return "proxy address is invalid"
	default:
		return "proxy configuration is invalid"
	}
}
