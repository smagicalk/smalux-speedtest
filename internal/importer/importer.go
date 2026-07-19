package importer

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
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

// ParseLink 将一条分享链接规范化为 sing-box outbound JSON。
// 返回的 ProxySpec.Outbound 可能包含认证信息，属于敏感任务数据；Server/Port/Name 等
// 字段用于调度与展示，但服务端持久化时仍应使用单独的 MaskedAddress。
func ParseLink(link string) (model.ProxySpec, error) {
	// VMess 使用“Base64(JSON)”而非标准 URI authority；Shadowsocks 又存在 SIP002 的
	// 多种 Base64 位置，因此二者必须在 net/url 通用解析前分流。
	if strings.HasPrefix(strings.ToLower(link), "vmess://") {
		return parseVMess(link)
	}
	if strings.HasPrefix(strings.ToLower(link), "ss://") {
		return parseShadowsocks(link)
	}
	if strings.HasPrefix(strings.ToLower(link), "ssr://") {
		// 明确拒绝已移除协议，让订阅导入结果说明原因，而不是笼统报 unsupported。
		return model.ProxySpec{}, errors.New("ssr was removed by sing-box 1.13 and cannot be tested")
	}
	u, err := url.Parse(link)
	if err != nil {
		return model.ProxySpec{}, fmt.Errorf("invalid URI: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if !supportedSchemes[scheme] {
		return model.ProxySpec{}, fmt.Errorf("unsupported protocol %q", scheme)
	}
	if u.Hostname() == "" {
		return model.ProxySpec{}, errors.New("missing server")
	}
	port, err := parsePort(u)
	if err != nil {
		return model.ProxySpec{}, err
	}
	// Fragment 不参与网络连接，按分享链接惯例作为用户可读节点名。
	name, _ := url.PathUnescape(strings.TrimPrefix(u.Fragment, "#"))
	if name == "" {
		name = scheme + "-" + u.Hostname()
	}
	q := u.Query()
	// 所有协议使用同一 tag，执行器会将该 outbound 作为本次测速的唯一代理出口。
	outbound := map[string]any{"tag": "proxy", "server": u.Hostname(), "server_port": port}

	switch scheme {
	case "vless":
		// VLESS 的 URI username 是 UUID；flow 和 V2Ray transport/TLS 由查询参数补充。
		outbound["type"] = "vless"
		outbound["uuid"] = username(u)
		put(outbound, "flow", q.Get("flow"))
		applyV2Ray(outbound, q)
	case "trojan":
		// Trojan 分享链接通常省略 security=tls，但协议默认要求 TLS，因此主动补齐。
		outbound["type"] = "trojan"
		outbound["password"] = username(u)
		if q.Get("security") == "" {
			q.Set("security", "tls")
		}
		applyV2Ray(outbound, q)
	case "hysteria", "hysteria2", "hy2":
		// hy2 是 hysteria2 的常见别名；两个协议在 sing-box 中的认证和混淆结构不同。
		if scheme == "hysteria" {
			outbound["type"] = "hysteria"
			outbound["auth_str"] = username(u)
			putInt(outbound, "up_mbps", first(q, "upmbps", "up"))
			putInt(outbound, "down_mbps", first(q, "downmbps", "down"))
			put(outbound, "obfs", q.Get("obfs"))
		} else {
			outbound["type"] = "hysteria2"
			outbound["password"] = username(u)
			if q.Get("obfs") != "" {
				outbound["obfs"] = map[string]any{"type": q.Get("obfs"), "password": first(q, "obfs-password", "obfs_password")}
			}
		}
		// Hysteria 系列本身依赖 TLS，所以即使链接未写 security 也强制生成 TLS 配置。
		applyTLS(outbound, q, u.Hostname(), true)
	case "tuic":
		// TUIC 使用 username/password 分别携带 UUID 与令牌，并接受不同客户端常见的
		// 下划线或连字符查询参数命名。
		outbound["type"] = "tuic"
		outbound["uuid"] = username(u)
		if password, ok := u.User.Password(); ok {
			outbound["password"] = password
		}
		put(outbound, "congestion_control", first(q, "congestion_control", "congestion-controller"))
		put(outbound, "udp_relay_mode", first(q, "udp_relay_mode", "udp-relay-mode"))
		applyTLS(outbound, q, u.Hostname(), true)
	case "socks", "socks5":
		// socks:// 与 socks5:// 均规范化成 sing-box type=socks, version=5。
		outbound["type"] = "socks"
		outbound["version"] = "5"
		outbound["username"] = username(u)
		if password, ok := u.User.Password(); ok {
			outbound["password"] = password
		}
	case "http", "https":
		// https:// 代表到上游 HTTP 代理本身的 TLS 连接，不是目标网站是否使用 HTTPS。
		outbound["type"] = "http"
		outbound["username"] = username(u)
		if password, ok := u.User.Password(); ok {
			outbound["password"] = password
		}
		if scheme == "https" {
			applyTLS(outbound, q, u.Hostname(), true)
		}
	case "anytls":
		// AnyTLS 分享格式将 URI username 作为密码，且传输层固定启用 TLS。
		outbound["type"] = "anytls"
		outbound["password"] = username(u)
		applyTLS(outbound, q, u.Hostname(), true)
	case "ssh":
		// 当前分享格式只提取口令认证；其他 SSH 密钥字段若未来支持需显式映射。
		outbound["type"] = "ssh"
		outbound["user"] = username(u)
		if password, ok := u.User.Password(); ok {
			outbound["password"] = password
		}
	}
	return makeSpec(name, fmt.Sprint(outbound["type"]), u.Hostname(), port, outbound)
}

// parseVMess 解析 VMess 传统 Base64 JSON 分享格式并转成 sing-box 字段。
// VMess JSON 在不同客户端中会把端口、alterId 等写成字符串或数字，stringValue
// 负责统一读取；网络传输与 TLS 参数再复用 V2Ray 公共规范化逻辑。
func parseVMess(link string) (model.ProxySpec, error) {
	raw, err := decodeBase64(strings.TrimPrefix(link, "vmess://"))
	if err != nil {
		return model.ProxySpec{}, fmt.Errorf("invalid vmess base64: %w", err)
	}
	var source map[string]any
	if err := json.Unmarshal([]byte(raw), &source); err != nil {
		return model.ProxySpec{}, fmt.Errorf("invalid vmess JSON: %w", err)
	}
	server := stringValue(source["add"])
	port64, err := strconv.ParseUint(stringValue(source["port"]), 10, 16)
	if err != nil || server == "" {
		return model.ProxySpec{}, errors.New("vmess server or port is invalid")
	}
	// scy 为空时使用 sing-box/VMess 通用的 auto，而不是把空字符串交给运行时。
	outbound := map[string]any{
		"type": "vmess", "tag": "proxy", "server": server, "server_port": uint16(port64),
		"uuid": stringValue(source["id"]), "security": stringValue(source["scy"]),
	}
	if outbound["security"] == "" {
		outbound["security"] = "auto"
	}
	if alterID, err := strconv.Atoi(stringValue(source["aid"])); err == nil && alterID != 0 {
		outbound["alter_id"] = alterID
	}
	// 将旧 VMess JSON 键转换成与 VLESS/Trojan 分享链接相同的查询参数视图，避免
	// WebSocket、gRPC、HTTP transport 与 TLS 映射出现两套实现。
	q := make(url.Values)
	q.Set("type", stringValue(source["net"]))
	q.Set("host", stringValue(source["host"]))
	q.Set("path", stringValue(source["path"]))
	q.Set("security", stringValue(source["tls"]))
	q.Set("sni", stringValue(source["sni"]))
	q.Set("fp", stringValue(source["fp"]))
	applyV2Ray(outbound, q)
	name := stringValue(source["ps"])
	if name == "" {
		name = "vmess-" + server
	}
	return makeSpec(name, "vmess", server, uint16(port64), outbound)
}

// parseShadowsocks 兼容 SIP002 及旧版 Shadowsocks 分享格式。
//
// 支持的凭据布局包括：
//   - ss://Base64(method:password)@host:port
//   - ss://method:password@host:port
//   - ss://Base64(method:password@host:port)
//
// fragment 用作节点名，plugin 查询参数按 SIP002 拆成 plugin/plugin_opts。
func parseShadowsocks(link string) (model.ProxySpec, error) {
	rest := strings.TrimPrefix(link, "ss://")
	// 在 Base64/authority 拆分前先移除 fragment 和 query，防止其中的 @、# 等字符
	// 干扰凭据识别；URL 解码只用于用户可读名称与标准查询参数。
	fragment := ""
	if index := strings.Index(rest, "#"); index >= 0 {
		fragment, _ = url.PathUnescape(rest[index+1:])
		rest = rest[:index]
	}
	query := ""
	if index := strings.Index(rest, "?"); index >= 0 {
		query = rest[index+1:]
		rest = rest[:index]
	}
	// 没有 @ 表示整段 authority 使用旧式 Base64 编码。
	if !strings.Contains(rest, "@") {
		decoded, err := decodeBase64(rest)
		if err != nil {
			return model.ProxySpec{}, errors.New("invalid shadowsocks credential")
		}
		rest = decoded
	}
	parts := strings.SplitN(rest, "@", 2)
	if len(parts) != 2 {
		return model.ProxySpec{}, errors.New("invalid shadowsocks URI")
	}
	credential := parts[0]
	// SIP002 推荐仅 Base64 编码 userinfo；若解码失败则兼容明文 method:password。
	if decoded, err := decodeBase64(credential); err == nil && strings.Contains(decoded, ":") {
		credential = decoded
	}
	credentialParts := strings.SplitN(credential, ":", 2)
	if len(credentialParts) != 2 {
		return model.ProxySpec{}, errors.New("invalid shadowsocks method/password")
	}
	// 借助 net/url 正确处理域名、端口和带方括号的 IPv6 地址。
	hostURL, err := url.Parse("ss://" + parts[1])
	if err != nil {
		return model.ProxySpec{}, err
	}
	port, err := parsePort(hostURL)
	if err != nil {
		return model.ProxySpec{}, err
	}
	outbound := map[string]any{
		"type": "shadowsocks", "tag": "proxy", "server": hostURL.Hostname(), "server_port": port,
		"method": credentialParts[0], "password": credentialParts[1],
	}
	values, _ := url.ParseQuery(query)
	if plugin := values.Get("plugin"); plugin != "" {
		// 第一个分号分隔插件名与原样选项串；选项的内部语法由 sing-box 插件处理。
		pluginParts := strings.SplitN(plugin, ";", 2)
		outbound["plugin"] = pluginParts[0]
		if len(pluginParts) == 2 {
			outbound["plugin_opts"] = pluginParts[1]
		}
	}
	if fragment == "" {
		fragment = "ss-" + hostURL.Hostname()
	}
	return makeSpec(fragment, "shadowsocks", hostURL.Hostname(), port, outbound)
}

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

// makeSpec 序列化规范化后的 outbound，并补充分布式任务使用的独立随机 ID。
// Outbound 原样包含运行代理所需的秘密，不应写入 Store.results 或直接返回管理页面。
func makeSpec(name, protocol, server string, port uint16, outbound map[string]any) (model.ProxySpec, error) {
	raw, err := json.Marshal(outbound)
	if err != nil {
		return model.ProxySpec{}, err
	}
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

// decodeBase64 依次兼容标准/URL-safe 字母表及有填充/无填充形式。
// 分享链接生态并不统一保留末尾 '='，因此不能只使用一种 Encoding。
func decodeBase64(value string) (string, error) {
	value = strings.TrimSpace(value)
	encodings := []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding}
	for _, encoding := range encodings {
		if decoded, err := encoding.DecodeString(value); err == nil {
			return string(decoded), nil
		}
	}
	return "", errors.New("invalid base64")
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
