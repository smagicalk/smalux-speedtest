package importer

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"smalux-speedtest/internal/model"
)

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
		// 默认名称会随测速结果长期保存。不要把真实服务器地址复制到名称中；
		// 管理员明确提供的 fragment 仍作为可辨识的报告名称保留。
		name = scheme + "-node"
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
		// VLESS clients commonly spell this option packetEncoding. Keep the
		// sing-box packet_encoding field when present; it is relevant to UDP
		// packet mode and harmless for the TCP-based Speedtest requests.
		put(outbound, "packet_encoding", first(q, "packetEncoding", "packet_encoding"))
		if err := applyV2Ray(outbound, q); err != nil {
			return model.ProxySpec{}, err
		}
	case "trojan":
		// Trojan 分享链接通常省略 security=tls，但协议默认要求 TLS，因此主动补齐。
		outbound["type"] = "trojan"
		outbound["password"] = username(u)
		if q.Get("security") == "" {
			q.Set("security", "tls")
		}
		if err := applyV2Ray(outbound, q); err != nil {
			return model.ProxySpec{}, err
		}
	case "hysteria", "hysteria2", "hy2":
		// hy2 是 hysteria2 的常见别名；两个协议在 sing-box 中的认证和混淆结构不同。
		if scheme == "hysteria" {
			outbound["type"] = "hysteria"
			auth := username(u)
			if queryAuth := first(q, "auth", "auth_str"); queryAuth != "" {
				auth = queryAuth
			}
			outbound["auth_str"] = auth
			putInt(outbound, "up_mbps", first(q, "upmbps", "up"))
			putInt(outbound, "down_mbps", first(q, "downmbps", "down"))
			if _, upOK := outbound["up_mbps"]; !upOK {
				return model.ProxySpec{}, errors.New("hysteria upload speed is invalid")
			}
			if _, downOK := outbound["down_mbps"]; !downOK {
				return model.ProxySpec{}, errors.New("hysteria download speed is invalid")
			}
			// Hysteria v1 links commonly use obfs=xplus&obfsParam=secret,
			// while sing-box expects only the XPlus password in obfs.
			obfs := first(q, "obfsParam", "obfs-param", "obfs_password")
			if obfs == "" && !strings.EqualFold(q.Get("obfs"), "xplus") {
				obfs = q.Get("obfs")
			}
			put(outbound, "obfs", obfs)
		} else {
			outbound["type"] = "hysteria2"
			password := username(u)
			if queryPassword := first(q, "auth", "password"); queryPassword != "" {
				password = queryPassword
			}
			outbound["password"] = password
			putInt(outbound, "up_mbps", first(q, "upmbps", "up"))
			putInt(outbound, "down_mbps", first(q, "downmbps", "down"))
			if q.Get("obfs") != "" {
				outbound["obfs"] = map[string]any{"type": q.Get("obfs"), "password": first(q, "obfs-password", "obfs_password")}
			}
		}
		if err := applyHysteriaPortHopping(outbound, q); err != nil {
			return model.ProxySpec{}, err
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

// applyHysteriaPortHopping maps the common URI aliases used by Hysteria v1/v2
// clients to sing-box's server_ports and hop_interval fields.
func applyHysteriaPortHopping(outbound map[string]any, q url.Values) error {
	if ports := first(q, "mport", "ports", "server_ports"); ports != "" {
		parts := strings.Split(ports, ",")
		cleaned := make([]string, 0, len(parts))
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			separator := strings.IndexAny(part, "-:")
			startText, endText := part, part
			if separator >= 0 {
				startText, endText = part[:separator], part[separator+1:]
			}
			start, startErr := strconv.ParseUint(startText, 10, 16)
			end, endErr := strconv.ParseUint(endText, 10, 16)
			if startErr != nil || endErr != nil || start == 0 || end == 0 || start > end {
				return errors.New("hysteria port range is invalid")
			}
			cleaned = append(cleaned, strconv.FormatUint(start, 10)+":"+strconv.FormatUint(end, 10))
		}
		if len(cleaned) > 0 {
			outbound["server_ports"] = cleaned
		}
	}
	put(outbound, "hop_interval", first(q, "hop_interval", "hop-interval"))
	return nil
}
