package importer

import (
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"

	"smalux-speedtest/internal/model"
)

// makeSpec 序列化规范化后的 outbound，并补充分布式任务使用的独立随机 ID。
// Outbound 原样包含运行代理所需的秘密，不应写入 Store.results 或直接返回管理页面。
func makeSpec(name, protocol, server string, port uint16, outbound map[string]any) (model.ProxySpec, error) {
	raw, err := json.Marshal(outbound)
	if err != nil {
		return model.ProxySpec{}, err
	}
	// URI fragments and VMess ps values are user-controlled labels. Normalize at the
	// importer boundary so every protocol-specific parser gets identical privacy rules.
	name = model.NormalizeProxyName(protocol, name, server)
	return model.ProxySpec{ID: model.NewID(), Name: name, Protocol: protocol, Server: server, Port: port, Outbound: raw}, nil
}

// parsePort 解析并校验 1..65535 端口。
// 只有 HTTP/HTTPS 具有无歧义默认端口；其他代理协议缺少端口时直接报错，避免猜测。
func parsePort(u *url.URL) (uint16, error) {
	portText := u.Port()
	if portText == "" {
		switch strings.ToLower(u.Scheme) {
		case "http":
			return 80, nil
		case "https":
			return 443, nil
		default:
			return 0, errors.New("missing port")
		}
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 {
		return 0, errors.New("invalid port")
	}
	return uint16(port), nil
}

// username 读取并 URL 解码 URI userinfo 的用户名部分。
// 对 VLESS/TUIC 它通常是 UUID，对 Trojan/AnyTLS 是密码，对 SOCKS/HTTP/SSH 是用户名；
// 具体语义由上层协议分支决定。
func username(u *url.URL) string {
	if u.User == nil {
		return ""
	}
	value, _ := url.PathUnescape(u.User.Username())
	return value
}

// put 仅在非空时写入可选字符串，防止空值覆盖 sing-box 自身默认行为。
func put(target map[string]any, key, value string) {
	if value != "" {
		target[key] = value
	}
}

// putInt 将正整数字符串写入 outbound；缺失、非数字及非正数均按“未配置”处理。
func putInt(target map[string]any, key, value string) {
	if parsed, err := strconv.Atoi(value); err == nil && parsed > 0 {
		target[key] = parsed
	}
}

// first 返回查询参数别名列表中第一个非空值，用于兼容不同客户端的字段命名。
func first(values url.Values, keys ...string) string {
	for _, key := range keys {
		if value := values.Get(key); value != "" {
			return value
		}
	}
	return ""
}

// truthy 识别分享链接常见的布尔真值，比较不区分大小写。
func truthy(value string) bool {
	switch strings.ToLower(value) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

// stringValue 将 VMess JSON 中常见的字符串或 JSON number 统一成文本。
// 其他类型（对象、数组、布尔值、null）不符合预期，按空值处理并由后续校验决定报错。
func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	default:
		return ""
	}
}
