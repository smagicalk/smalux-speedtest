package clientapp

import (
	"context"
	"encoding/json"
	"errors"
	"net"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter/endpoint"
	"github.com/sagernet/sing-box/adapter/inbound"
	"github.com/sagernet/sing-box/adapter/outbound"
	boxservice "github.com/sagernet/sing-box/adapter/service"
	"github.com/sagernet/sing-box/dns"
	"github.com/sagernet/sing-box/dns/transport/local"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/protocol/anytls"
	"github.com/sagernet/sing-box/protocol/direct"
	boxhttp "github.com/sagernet/sing-box/protocol/http"
	"github.com/sagernet/sing-box/protocol/hysteria"
	"github.com/sagernet/sing-box/protocol/hysteria2"
	"github.com/sagernet/sing-box/protocol/shadowsocks"
	"github.com/sagernet/sing-box/protocol/socks"
	"github.com/sagernet/sing-box/protocol/ssh"
	"github.com/sagernet/sing-box/protocol/trojan"
	"github.com/sagernet/sing-box/protocol/tuic"
	"github.com/sagernet/sing-box/protocol/vless"
	"github.com/sagernet/sing-box/protocol/vmess"
	SJSON "github.com/sagernet/sing/common/json"
	M "github.com/sagernet/sing/common/metadata"
)

// startBox 将单条 sing-box outbound JSON 包装为可启动的最小配置。
//
// 配置固定把该出站标记为最终路由 proxy。返回的 dialer 是实例内同名 outbound，调用方
// 可直接用它建立目标 TCP 连接。成功时实例所有权交给调用方；任何初始化失败都会在
// 返回前关闭已创建的实例。
func startBox(ctx context.Context, outboundJSON json.RawMessage) (*box.Box, interface {
	DialContext(context.Context, string, M.Socksaddr) (net.Conn, error)
}, error) {
	configuration := struct {
		Log       map[string]any    `json:"log"`
		Outbounds []json.RawMessage `json:"outbounds"`
		Route     map[string]any    `json:"route"`
	}{
		// 代理配置可能包含敏感字段，因此禁用 sing-box 自身日志；运行状态由客户端日志负责。
		Log: map[string]any{"disabled": true}, Outbounds: []json.RawMessage{outboundJSON}, Route: map[string]any{"final": "proxy"},
	}
	raw, err := json.Marshal(configuration)
	if err != nil {
		return nil, nil, err
	}
	boxContext := minimalBoxContext(ctx)
	var options option.Options
	if err := SJSON.UnmarshalContext(boxContext, raw, &options); err != nil {
		return nil, nil, err
	}
	instance, err := box.New(box.Options{Options: options, Context: boxContext})
	if err != nil {
		return nil, nil, err
	}
	if err := instance.Start(); err != nil {
		instance.Close()
		return nil, nil, err
	}
	outbound, found := instance.Outbound().Outbound("proxy")
	if !found {
		instance.Close()
		return nil, nil, errors.New("proxy outbound not found")
	}
	return instance, outbound, nil
}

// minimalBoxContext 构造仅包含本项目支持协议的 sing-box 注册表。
//
// sing-box 使用 context 注入协议工厂。若没有显式注册，对应 Outbound JSON 即使语法
// 正确也无法实例化。这里不引入完整 include 包，而是列出导入器支持的协议，减少无关
// 功能和平台依赖，同时保留 direct 供测试及基础路由使用、本地 DNS transport 供域名
// 解析使用。新增代理协议时必须同步更新此注册表和导入器。
func minimalBoxContext(ctx context.Context) context.Context {
	inboundRegistry := inbound.NewRegistry()
	outboundRegistry := outbound.NewRegistry()
	dnsRegistry := dns.NewTransportRegistry()

	local.RegisterTransport(dnsRegistry)
	// 基础出站与传统 TCP 代理协议。
	direct.RegisterOutbound(outboundRegistry)
	socks.RegisterOutbound(outboundRegistry)
	boxhttp.RegisterOutbound(outboundRegistry)
	shadowsocks.RegisterOutbound(outboundRegistry)
	// 基于 TLS/用户身份的代理协议。
	vmess.RegisterOutbound(outboundRegistry)
	trojan.RegisterOutbound(outboundRegistry)
	vless.RegisterOutbound(outboundRegistry)
	ssh.RegisterOutbound(outboundRegistry)
	anytls.RegisterOutbound(outboundRegistry)
	// 以 UDP/QUIC 为底层传输的高性能代理协议。
	hysteria.RegisterOutbound(outboundRegistry)
	hysteria2.RegisterOutbound(outboundRegistry)
	tuic.RegisterOutbound(outboundRegistry)
	return box.Context(
		ctx,
		inboundRegistry,
		outboundRegistry,
		endpoint.NewRegistry(),
		dnsRegistry,
		boxservice.NewRegistry(),
	)
}
