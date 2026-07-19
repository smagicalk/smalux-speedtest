package clientapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"time"

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
	"github.com/showwin/speedtest-go/speedtest"

	"smalux-speedtest/internal/model"
)

// Executor 将 Assignment 中的 sing-box 出站配置转换为实际代理连接，并通过该连接运行
// speedtest-go。Executor 本身不保存可变状态；每次 Execute 都创建独立的 sing-box、
// HTTP Transport 和 speedtest Client。
type Executor struct{}

// NewExecutor 创建测速执行器。
func NewExecutor() *Executor { return &Executor{} }

// Execute 按 Assignment.Proxies 的顺序执行完整测速任务。
//
// 每个代理独立启动和关闭 sing-box。某个代理初始化或获取测速节点失败时，会生成一条
// 带 Error 的 SpeedResult 并继续下一个代理，避免单个坏节点中止整批任务。ctx 取消后
// 不再开始新代理，已经进入的网络调用也会通过派生 context 尽快终止。
//
// progress 用于向调用方报告阶段和实时速率。speedtest-go 的速率回调可能在测速执行
// 期间频繁触发，因此实现应快速返回，且不应把耗时持久化操作直接放在回调中。
func (e *Executor) Execute(ctx context.Context, assignment model.Assignment, progress func(model.Progress)) []model.SpeedResult {
	results := make([]model.SpeedResult, 0, len(assignment.Proxies)*assignment.TopN)
	for index, proxy := range assignment.Proxies {
		if err := ctx.Err(); err != nil {
			break
		}
		progress(model.Progress{
			TaskID: assignment.TaskID, ProxyID: proxy.ID, ProxyName: proxy.Name, Phase: "初始化代理",
			Message: proxy.Protocol, Current: index, Total: len(assignment.Proxies),
		})
		proxyResults, err := e.testProxy(ctx, assignment, proxy, progress)
		if err != nil {
			results = append(results, model.SpeedResult{
				TaskID: assignment.TaskID, ProxyID: proxy.ID, ProxyName: proxy.Name, Protocol: proxy.Protocol,
				MaskedAddress: model.MaskAddress(proxy.Server, proxy.Port), Error: err.Error(), CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
			})
			continue
		}
		results = append(results, proxyResults...)
	}
	return results
}

// testProxy 使用一个代理完成“启动 sing-box -> 获取候选节点 -> HTTP 延迟筛选 ->
// Top N 下载/上传测速”的全过程。返回结果中的地址已脱敏，不包含 Outbound 内的凭据。
func (e *Executor) testProxy(ctx context.Context, assignment model.Assignment, proxy model.ProxySpec, progress func(model.Progress)) ([]model.SpeedResult, error) {
	// 每个代理使用独立实例，既隔离连接池和协议状态，也确保测试结束后释放底层会话。
	instance, outbound, err := startBox(ctx, proxy.Outbound)
	if err != nil {
		return nil, fmt.Errorf("start sing-box: %w", err)
	}
	defer instance.Close()

	// speedtest-go 发起的是普通 HTTP/HTTPS 请求。自定义 DialContext 把所有 TCP 建连
	// 交给 tag=proxy 的 sing-box outbound，从而让服务器列表、延迟、下载和上传流量
	// 全部经过被测代理，而不是误走客户端机器的直连网络。
	transport := &http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: time.Second,
		DialContext: func(dialCtx context.Context, network, address string) (net.Conn, error) {
			return outbound.DialContext(dialCtx, "tcp", M.ParseSocksaddr(address))
		},
	}
	defer transport.CloseIdleConnections()
	httpClient := &http.Client{Transport: transport, Timeout: 45 * time.Second}
	// 服务端正常会把 Threads 校验在 1..32；这里再次防御不可信或旧版任务消息。
	threads := assignment.Threads
	if threads < 1 || threads > 32 {
		threads = 4
	}
	client := speedtest.New(
		speedtest.WithUserConfig(&speedtest.UserConfig{PingMode: speedtest.HTTP, MaxConnections: threads}),
		speedtest.WithDoer(httpClient),
	)

	progress(model.Progress{TaskID: assignment.TaskID, ProxyID: proxy.ID, ProxyName: proxy.Name, Phase: "获取测速节点", Message: "Speedtest.net"})
	servers, err := client.FetchServerListContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch speedtest servers: %w", err)
	}
	// 只对前 CandidateCount 个候选节点做 Ping，限制探测耗时和外部请求数量。
	if len(servers) > assignment.CandidateCount {
		servers = servers[:assignment.CandidateCount]
	}
	if len(servers) == 0 {
		return nil, errors.New("no speedtest server available")
	}

	// 每个候选节点拥有独立 12 秒上限。单点失败不会中止代理测试，只从可用集合剔除。
	available := make(speedtest.Servers, 0, len(servers))
	for index, server := range servers {
		progress(model.Progress{
			TaskID: assignment.TaskID, ProxyID: proxy.ID, ProxyName: proxy.Name, Phase: "延迟检测",
			Message: server.Name + " / " + server.Sponsor, Current: index + 1, Total: len(servers),
		})
		pingCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
		err := server.PingTestContext(pingCtx, nil)
		cancel()
		if err == nil && server.Latency > 0 && server.Latency < speedtest.PingTimeout {
			available = append(available, server)
		}
	}
	if len(available) == 0 {
		return nil, errors.New("all speedtest latency checks failed")
	}
	// 稳定排序在延迟相同时保留 API 返回顺序，随后只对最低延迟的 Top N 节点传输大流量。
	sort.SliceStable(available, func(i, j int) bool { return available[i].Latency < available[j].Latency })
	selected := assignment.TopN
	if selected > len(available) {
		selected = len(available)
	}

	results := make([]model.SpeedResult, 0, selected)
	for index, server := range available[:selected] {
		started := time.Now()
		base := model.Progress{
			TaskID: assignment.TaskID, ProxyID: proxy.ID, ProxyName: proxy.Name,
			Message: server.Name + " / " + server.Sponsor, Current: index + 1, Total: selected,
		}
		client.SetCallbackDownload(func(rate speedtest.ByteRate) {
			update := base
			update.Phase = "下载测速"
			update.RateBPS = float64(rate) * 8
			progress(update)
		})
		base.Phase = "下载测速"
		progress(base)
		// 下载和上传分别限制为 45 秒；父任务取消会比局部超时更早生效。
		downloadCtx, cancelDownload := context.WithTimeout(ctx, 45*time.Second)
		downloadErr := server.DownloadTestContext(downloadCtx)
		cancelDownload()

		client.SetCallbackUpload(func(rate speedtest.ByteRate) {
			update := base
			update.Phase = "上传测速"
			update.RateBPS = float64(rate) * 8
			progress(update)
		})
		base.Phase = "上传测速"
		progress(base)
		uploadCtx, cancelUpload := context.WithTimeout(ctx, 45*time.Second)
		uploadErr := server.UploadTestContext(uploadCtx)
		cancelUpload()

		result := model.SpeedResult{
			TaskID: assignment.TaskID, ProxyID: proxy.ID, ProxyName: proxy.Name, Protocol: proxy.Protocol,
			MaskedAddress: model.MaskAddress(proxy.Server, proxy.Port), SpeedServerID: server.ID,
			SpeedServerName: server.Name, SpeedServerHost: server.Host, Country: server.Country, Sponsor: server.Sponsor,
			LatencyMS: float64(server.Latency) / float64(time.Millisecond), JitterMS: float64(server.Jitter) / float64(time.Millisecond),
			DownloadBPS: float64(server.DLSpeed) * 8, UploadBPS: float64(server.ULSpeed) * 8,
			DurationMS: time.Since(started).Milliseconds(), CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		}
		if downloadErr != nil || uploadErr != nil {
			// 保留已测得的延迟和速率，同时把一个或两个方向的错误合并到同一结果中。
			result.Error = errors.Join(downloadErr, uploadErr).Error()
		}
		results = append(results, result)
		// speedtest Client 会累计 Manager 状态；节点之间重置，避免前一个节点的统计量
		// 污染后一个节点的结果。
		client.Manager.Reset()
	}
	return results, nil
}

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
