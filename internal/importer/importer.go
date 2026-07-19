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
			// Error.Input 只保存经过截断的诊断文本，不把完整长分享链接带入 API 结果。
			result.Errors = append(result.Errors, model.ImportError{Line: index + 1, Input: redactInput(line), Error: err.Error()})
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

// redactInput 缩短导入错误中回显的长输入，降低完整凭据通过诊断信息扩散的风险。
//
// 这只是可用性导向的截断，并非密码学意义的完整脱敏：短链接会原样返回，长链接的
// 首尾仍可能含敏感片段。因此调用方不应把 ImportError.Input 写入公开日志或返回给
// 非管理员；真正持久化的测速结果必须使用独立生成的 MaskedAddress。
func redactInput(value string) string {
	if len(value) <= 32 {
		return value
	}
	return value[:16] + "..." + value[len(value)-8:]
}
