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

type Executor struct{}

func NewExecutor() *Executor { return &Executor{} }

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

func (e *Executor) testProxy(ctx context.Context, assignment model.Assignment, proxy model.ProxySpec, progress func(model.Progress)) ([]model.SpeedResult, error) {
	instance, outbound, err := startBox(ctx, proxy.Outbound)
	if err != nil {
		return nil, fmt.Errorf("start sing-box: %w", err)
	}
	defer instance.Close()

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
	if len(servers) > assignment.CandidateCount {
		servers = servers[:assignment.CandidateCount]
	}
	if len(servers) == 0 {
		return nil, errors.New("no speedtest server available")
	}

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
			result.Error = errors.Join(downloadErr, uploadErr).Error()
		}
		results = append(results, result)
		client.Manager.Reset()
	}
	return results, nil
}

func startBox(ctx context.Context, outboundJSON json.RawMessage) (*box.Box, interface {
	DialContext(context.Context, string, M.Socksaddr) (net.Conn, error)
}, error) {
	configuration := struct {
		Log       map[string]any    `json:"log"`
		Outbounds []json.RawMessage `json:"outbounds"`
		Route     map[string]any    `json:"route"`
	}{
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

func minimalBoxContext(ctx context.Context) context.Context {
	inboundRegistry := inbound.NewRegistry()
	outboundRegistry := outbound.NewRegistry()
	dnsRegistry := dns.NewTransportRegistry()
	local.RegisterTransport(dnsRegistry)
	direct.RegisterOutbound(outboundRegistry)
	socks.RegisterOutbound(outboundRegistry)
	boxhttp.RegisterOutbound(outboundRegistry)
	shadowsocks.RegisterOutbound(outboundRegistry)
	vmess.RegisterOutbound(outboundRegistry)
	trojan.RegisterOutbound(outboundRegistry)
	vless.RegisterOutbound(outboundRegistry)
	ssh.RegisterOutbound(outboundRegistry)
	anytls.RegisterOutbound(outboundRegistry)
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
