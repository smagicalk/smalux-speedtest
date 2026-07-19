package importer

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"smalux-speedtest/internal/model"
)

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
