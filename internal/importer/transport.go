package importer

import (
	"fmt"
	"net/url"
	"strings"
)

// applyV2Ray 将 VLESS、Trojan、VMess 共用的 TLS 与传输层参数写入 outbound。
// 未识别或空 type 表示使用协议默认 TCP，不额外生成 transport，避免无意义配置改变行为。
func applyV2Ray(outbound map[string]any, q url.Values) {
	applyTLS(outbound, q, fmt.Sprint(outbound["server"]), false)
	typeName := strings.ToLower(q.Get("type"))
	switch typeName {
	case "ws", "websocket":
		// WebSocket Host 是 HTTP 请求头，而 path 是 transport 顶层字段。
		headers := map[string]any{}
		if host := q.Get("host"); host != "" {
			headers["Host"] = host
		}
		transport := map[string]any{"type": "ws", "path": q.Get("path")}
		if len(headers) > 0 {
			transport["headers"] = headers
		}
		outbound["transport"] = transport
	case "grpc":
		// 同时兼容分享生态中的 serviceName 与 sing-box 风格 service_name。
		outbound["transport"] = map[string]any{"type": "grpc", "service_name": first(q, "serviceName", "service_name")}
	case "http", "h2":
		// HTTP transport 的 host 是列表；分享链接通常以逗号编码多个候选值。
		transport := map[string]any{"type": "http", "path": q.Get("path")}
		if host := q.Get("host"); host != "" {
			transport["host"] = strings.Split(host, ",")
		}
		outbound["transport"] = transport
	case "httpupgrade":
		transport := map[string]any{"type": "httpupgrade", "path": q.Get("path")}
		put(transport, "host", q.Get("host"))
		outbound["transport"] = transport
	}
}

// applyTLS 将常见分享链接的 TLS/uTLS/REALITY 参数规范化为 sing-box 结构。
// force 用于协议本身必需 TLS 的场景；否则仅 security=tls/reality 时启用，确保普通
// TCP 链接不会被意外升级。insecure 只在链接明确给出真值时设置。
func applyTLS(outbound map[string]any, q url.Values, defaultServerName string, force bool) {
	security := strings.ToLower(first(q, "security", "tls"))
	if !force && security != "tls" && security != "reality" {
		return
	}
	tls := map[string]any{"enabled": true}
	// SNI 参数存在多种命名；均缺失时回落到连接服务器域名。
	serverName := first(q, "sni", "peer", "server_name")
	if serverName == "" {
		serverName = defaultServerName
	}
	tls["server_name"] = serverName
	if truthy(first(q, "insecure", "allowInsecure", "allow_insecure")) {
		tls["insecure"] = true
	}
	if alpn := q.Get("alpn"); alpn != "" {
		tls["alpn"] = strings.Split(alpn, ",")
	}
	if fingerprint := first(q, "fp", "fingerprint"); fingerprint != "" {
		// 浏览器指纹由 sing-box uTLS 实现，导入器只做字段映射与启用开关。
		tls["utls"] = map[string]any{"enabled": true, "fingerprint": fingerprint}
	}
	if security == "reality" || q.Get("pbk") != "" {
		// 部分链接遗漏 security=reality 但携带 pbk，因此公钥本身也作为启用信号。
		tls["reality"] = map[string]any{"enabled": true, "public_key": first(q, "pbk", "public_key"), "short_id": first(q, "sid", "short_id")}
	}
	outbound["tls"] = tls
}
