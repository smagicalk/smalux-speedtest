package clientapp

import (
	"context"
	"net"
	"net/http"
	"sort"
	"time"

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
// 带 Error 的 SpeedResult 并结束当前工作单元，避免单个坏节点中止整个任务。ctx 取消后
// 不再开始新代理，已经进入的网络调用也会通过派生 context 尽快终止。
//
// progress 用于向调用方报告阶段和实时速率。speedtest-go 的速率回调可能在测速执行
// 期间频繁触发，因此实现应快速返回，且不应把耗时持久化操作直接放在回调中。
func (e *Executor) Execute(ctx context.Context, assignment model.Assignment, progress func(model.Progress)) []model.SpeedResult {
	results := make([]model.SpeedResult, 0, len(assignment.Proxies)*assignment.TopN)
	for _, proxy := range assignment.Proxies {
		if err := ctx.Err(); err != nil {
			break
		}
		progress(model.Progress{
			TaskID: assignment.TaskID, WorkID: assignment.WorkID, ProxyID: proxy.ID, ProxyName: proxy.Name, Phase: "初始化代理",
			Message: proxy.Protocol, Current: assignment.ProxyIndex, Total: assignment.ProxyTotal,
		})
		proxyResults, err := e.testProxy(ctx, assignment, proxy, progress)
		if err != nil {
			results = append(results, model.SpeedResult{
				TaskID: assignment.TaskID, WorkID: assignment.WorkID, ProxyID: proxy.ID, ProxyName: proxy.Name, Protocol: proxy.Protocol,
				MaskedAddress: model.MaskAddress(proxy.Server, proxy.Port), Error: executionResultError(err), CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
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
		return nil, executionError(errProxyInitialization, err)
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

	progress(model.Progress{TaskID: assignment.TaskID, WorkID: assignment.WorkID, ProxyID: proxy.ID, ProxyName: proxy.Name, Phase: "获取测速节点", Message: "Speedtest.net"})
	servers, err := client.FetchServerListContext(ctx)
	if err != nil {
		return nil, executionError(errSpeedServerDiscovery, err)
	}
	// 只对前 CandidateCount 个候选节点做 Ping，限制探测耗时和外部请求数量。
	if len(servers) > assignment.CandidateCount {
		servers = servers[:assignment.CandidateCount]
	}
	if len(servers) == 0 {
		return nil, errNoSpeedServer
	}

	// 每个候选节点拥有独立 12 秒上限。单点失败不会中止代理测试，只从可用集合剔除。
	available := make(speedtest.Servers, 0, len(servers))
	for index, server := range servers {
		progress(model.Progress{
			TaskID: assignment.TaskID, WorkID: assignment.WorkID, ProxyID: proxy.ID, ProxyName: proxy.Name, Phase: "延迟检测",
			Message: server.Name + " / " + server.Sponsor, Current: index + 1, Total: len(servers),
		})
		pingCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
		err := server.PingTestContext(pingCtx, nil)
		cancel()
		// speedtest-go uses PingTimeout (-1) as a failure sentinel.  A
		// successful latency is a positive duration; comparing it with the
		// sentinel in the opposite direction would reject every healthy server.
		if err == nil && usableLatency(server) {
			available = append(available, server)
		}
	}
	if len(available) == 0 {
		return nil, errLatencyChecks
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
			TaskID: assignment.TaskID, WorkID: assignment.WorkID, ProxyID: proxy.ID, ProxyName: proxy.Name,
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
			TaskID: assignment.TaskID, WorkID: assignment.WorkID, ProxyID: proxy.ID, ProxyName: proxy.Name, Protocol: proxy.Protocol,
			MaskedAddress: model.MaskAddress(proxy.Server, proxy.Port), SpeedServerID: server.ID,
			SpeedServerName: server.Name, SpeedServerHost: server.Host, Country: server.Country, Sponsor: server.Sponsor,
			LatencyMS: float64(server.Latency) / float64(time.Millisecond), JitterMS: float64(server.Jitter) / float64(time.Millisecond),
			DownloadBPS: float64(server.DLSpeed) * 8, UploadBPS: float64(server.ULSpeed) * 8,
			DurationMS: time.Since(started).Milliseconds(), CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		}
		if downloadErr != nil || uploadErr != nil {
			// 保留已测得的延迟和速率，但只记录失败方向。底层错误可能包含请求 URL、
			// 节点地址或网络栈细节，不得经 Result 进入 Server 数据库和导出文件。
			result.Error = transferResultError(downloadErr, uploadErr)
		}
		results = append(results, result)
		// speedtest Client 会累计 Manager 状态；节点之间重置，避免前一个节点的统计量
		// 污染后一个节点的结果。
		client.Manager.Reset()
	}
	return results, nil
}

// usableLatency reports whether speedtest-go recorded a real HTTP latency.
// Keep the sentinel check explicit: PingTimeout is currently -1, but relying
// only on a numeric comparison would make this rule fragile if the dependency
// changes its failure representation.
func usableLatency(server *speedtest.Server) bool {
	return server != nil && server.Latency != speedtest.PingTimeout && server.Latency > 0
}
