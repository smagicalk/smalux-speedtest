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

var supportedSchemes = map[string]bool{
	"ss": true, "ssr": true, "vmess": true, "vless": true, "trojan": true,
	"hysteria": true, "hysteria2": true, "hy2": true, "tuic": true,
	"socks": true, "socks5": true, "http": true, "https": true,
	"anytls": true, "ssh": true,
}

func Parse(content string) model.ImportResult {
	content = strings.TrimSpace(strings.TrimPrefix(content, "\ufeff"))
	if decoded, ok := decodeSubscription(content); ok {
		content = decoded
	}

	var result model.ImportResult
	seen := make(map[string]bool)
	for index, line := range strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		proxy, err := ParseLink(line)
		if err != nil {
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

func ParseLink(link string) (model.ProxySpec, error) {
	if strings.HasPrefix(strings.ToLower(link), "vmess://") {
		return parseVMess(link)
	}
	if strings.HasPrefix(strings.ToLower(link), "ss://") {
		return parseShadowsocks(link)
	}
	if strings.HasPrefix(strings.ToLower(link), "ssr://") {
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
	name, _ := url.PathUnescape(strings.TrimPrefix(u.Fragment, "#"))
	if name == "" {
		name = scheme + "-" + u.Hostname()
	}
	q := u.Query()
	outbound := map[string]any{"tag": "proxy", "server": u.Hostname(), "server_port": port}

	switch scheme {
	case "vless":
		outbound["type"] = "vless"
		outbound["uuid"] = username(u)
		put(outbound, "flow", q.Get("flow"))
		applyV2Ray(outbound, q)
	case "trojan":
		outbound["type"] = "trojan"
		outbound["password"] = username(u)
		if q.Get("security") == "" {
			q.Set("security", "tls")
		}
		applyV2Ray(outbound, q)
	case "hysteria", "hysteria2", "hy2":
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
		applyTLS(outbound, q, u.Hostname(), true)
	case "tuic":
		outbound["type"] = "tuic"
		outbound["uuid"] = username(u)
		if password, ok := u.User.Password(); ok {
			outbound["password"] = password
		}
		put(outbound, "congestion_control", first(q, "congestion_control", "congestion-controller"))
		put(outbound, "udp_relay_mode", first(q, "udp_relay_mode", "udp-relay-mode"))
		applyTLS(outbound, q, u.Hostname(), true)
	case "socks", "socks5":
		outbound["type"] = "socks"
		outbound["version"] = "5"
		outbound["username"] = username(u)
		if password, ok := u.User.Password(); ok {
			outbound["password"] = password
		}
	case "http", "https":
		outbound["type"] = "http"
		outbound["username"] = username(u)
		if password, ok := u.User.Password(); ok {
			outbound["password"] = password
		}
		if scheme == "https" {
			applyTLS(outbound, q, u.Hostname(), true)
		}
	case "anytls":
		outbound["type"] = "anytls"
		outbound["password"] = username(u)
		applyTLS(outbound, q, u.Hostname(), true)
	case "ssh":
		outbound["type"] = "ssh"
		outbound["user"] = username(u)
		if password, ok := u.User.Password(); ok {
			outbound["password"] = password
		}
	}
	return makeSpec(name, fmt.Sprint(outbound["type"]), u.Hostname(), port, outbound)
}

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

func parseShadowsocks(link string) (model.ProxySpec, error) {
	rest := strings.TrimPrefix(link, "ss://")
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
	if decoded, err := decodeBase64(credential); err == nil && strings.Contains(decoded, ":") {
		credential = decoded
	}
	credentialParts := strings.SplitN(credential, ":", 2)
	if len(credentialParts) != 2 {
		return model.ProxySpec{}, errors.New("invalid shadowsocks method/password")
	}
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

func applyV2Ray(outbound map[string]any, q url.Values) {
	applyTLS(outbound, q, fmt.Sprint(outbound["server"]), false)
	typeName := strings.ToLower(q.Get("type"))
	switch typeName {
	case "ws", "websocket":
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
		outbound["transport"] = map[string]any{"type": "grpc", "service_name": first(q, "serviceName", "service_name")}
	case "http", "h2":
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

func applyTLS(outbound map[string]any, q url.Values, defaultServerName string, force bool) {
	security := strings.ToLower(first(q, "security", "tls"))
	if !force && security != "tls" && security != "reality" {
		return
	}
	tls := map[string]any{"enabled": true}
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
		tls["utls"] = map[string]any{"enabled": true, "fingerprint": fingerprint}
	}
	if security == "reality" || q.Get("pbk") != "" {
		tls["reality"] = map[string]any{"enabled": true, "public_key": first(q, "pbk", "public_key"), "short_id": first(q, "sid", "short_id")}
	}
	outbound["tls"] = tls
}

func makeSpec(name, protocol, server string, port uint16, outbound map[string]any) (model.ProxySpec, error) {
	raw, err := json.Marshal(outbound)
	if err != nil {
		return model.ProxySpec{}, err
	}
	return model.ProxySpec{ID: model.NewID(), Name: name, Protocol: protocol, Server: server, Port: port, Outbound: raw}, nil
}

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

func username(u *url.URL) string {
	if u.User == nil {
		return ""
	}
	value, _ := url.PathUnescape(u.User.Username())
	return value
}

func put(target map[string]any, key, value string) {
	if value != "" {
		target[key] = value
	}
}

func putInt(target map[string]any, key, value string) {
	if parsed, err := strconv.Atoi(value); err == nil && parsed > 0 {
		target[key] = parsed
	}
}

func first(values url.Values, keys ...string) string {
	for _, key := range keys {
		if value := values.Get(key); value != "" {
			return value
		}
	}
	return ""
}

func truthy(value string) bool {
	switch strings.ToLower(value) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

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

func redactInput(value string) string {
	if len(value) <= 32 {
		return value
	}
	return value[:16] + "..." + value[len(value)-8:]
}
