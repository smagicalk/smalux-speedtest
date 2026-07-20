package importer

import (
	"errors"
	"net/url"
	"strings"

	"smalux-speedtest/internal/model"
)

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
		// 结果会保存节点名称；无 fragment 时使用匿名默认值，避免真实 host 通过
		// proxy_name 绕过 MaskedAddress 的脱敏策略。
		fragment = "ss-node"
	}
	return makeSpec(fragment, "shadowsocks", hostURL.Hostname(), port, outbound)
}
