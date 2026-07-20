package subscription

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// MaxSize 是单份订阅正文允许的最大字节数（5 MiB）。
// 限制按 Go HTTP Transport 解压后交给调用方读取的正文计算，足以容纳常规文本/Base64
// 订阅，同时也限制压缩率异常的响应解压后占用无界内存。
const MaxSize = 5 << 20

// Fetcher 使用专用 HTTP Client 拉取不受信任的订阅 URL。
// client 不导出，防止调用方替换掉自定义 DialContext、重定向策略或超时后绕过 SSRF
// 防护。Fetcher 可复用，其 Transport 也能安全复用连接。
type Fetcher struct {
	// client 固化安全 Transport、重定向复验和请求总超时，禁止调用方绕过策略。
	client *http.Client
}

// NewFetcher 创建带 SSRF 防护、重定向限制和总超时的订阅拉取器。
func NewFetcher() *Fetcher {
	// 建连超时限制 TCP 连接阶段；KeepAlive 仅控制底层连接保活，不延长请求总超时。
	dialer := &net.Dialer{Timeout: 8 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		// 明确禁用 ProxyFromEnvironment，防止 HTTP_PROXY/HTTPS_PROXY 将请求转发到
		// 未经过本地地址校验的代理，或借代理访问服务端内网。
		Proxy:                 nil,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   8 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			// address 由 net/http 生成，通常是 host:port。拆分后自行解析 DNS，避免将
			// 未检查的主机名再次交给 net.Dialer 解析。
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			if err != nil {
				return nil, err
			}
			// 采用“所有结果都必须允许”的保守策略：双栈或轮询 DNS 中只要混入一个
			// 私有/回环地址便拒绝整个请求，避免攻击者利用地址选择顺序绕过检查。
			for _, address := range addresses {
				if !publicAddress(address) {
					return nil, fmt.Errorf("subscription host resolves to blocked address %s", address)
				}
			}
			if len(addresses) == 0 {
				return nil, errors.New("subscription host has no address")
			}
			// 直接拨号刚刚校验过的首个 IP，关闭“校验时解析一次、拨号时再次解析”造成
			// DNS rebinding/TOCTOU 窗口。HTTP Host 与 TLS SNI 仍由原请求主机名生成。
			return dialer.DialContext(ctx, network, net.JoinHostPort(addresses[0].String(), port))
		},
	}
	return &Fetcher{client: &http.Client{
		Transport: transport,
		// Client.Timeout 覆盖连接、重定向、响应头和读取响应体的总过程，是各阶段超时
		// 之外的最终上限；调用方 context 可以设置更短的期限或主动取消。
		Timeout: 15 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// via 包含初始请求和此前各跳；即将进行第三次重定向时长度为 3 并被拒绝，
			// 因而最多跟随两跳，防止循环/超长链消耗连接和 DNS 查询资源。
			if len(via) >= 3 {
				return errors.New("too many subscription redirects")
			}
			// 每个 Location 都重新执行 scheme、userinfo、端口和字面 IP 校验；域名最终
			// 解析出的 IP 仍会在 DialContext 中检查，不能靠重定向跳到内网。
			return validateURL(req.URL)
		},
	}}
}

// Fetch 拉取一份订阅并返回原始响应正文。
//
// 它不解析 Base64 或代理协议，也不记录正文。任何非 2xx 状态、超过上限的正文、
// URL/解析地址策略违规或网络错误都会返回错误，不把部分响应当作有效订阅。
func (f *Fetcher) Fetch(ctx context.Context, rawURL string) (string, error) {
	// 首次请求与后续重定向使用同一套 validateURL 规则；TrimSpace 兼容表单输入。
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", errors.New("subscription URL is invalid")
	}
	if err := validateURL(parsed); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return "", errors.New("subscription URL is invalid")
	}
	// 固定 User-Agent 便于订阅服务识别客户端，且不携带服务器主机或管理员信息。
	req.Header.Set("User-Agent", "smalux-speedtest/1")
	resp, err := f.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		// net/http 的错误通常包含完整请求 URL，包括订阅鉴权 query。不要把底层
		// 错误传给控制面或日志。
		return "", errors.New("subscription request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("subscription returned HTTP %d", resp.StatusCode)
	}
	// Content-Length 可提前拒绝已声明的大响应，但不能单独信任：分块传输可能为 -1，
	// 服务端也可能声明错误长度，因此下面仍按实际读取字节做第二道限制。
	if resp.ContentLength > MaxSize {
		return "", errors.New("subscription is larger than 5 MiB")
	}
	// 多读一个字节用于区分“恰好 MaxSize”与“实际超限”，同时保证内存分配有硬上限。
	content, err := io.ReadAll(io.LimitReader(resp.Body, MaxSize+1))
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", errors.New("subscription response read failed")
	}
	if len(content) > MaxSize {
		return "", errors.New("subscription is larger than 5 MiB")
	}
	return string(content), nil
}

// validateURL 执行无需 DNS 的订阅 URL 静态校验。
//
// 这里只检查 URL 本身：仅允许 HTTP(S)，禁止 userinfo（避免 URL 内嵌凭据泄漏或自动
// 生成 Authorization），校验显式端口，并拒绝已是受限字面 IP 的主机。域名必须在
// DialContext 中解析后再次检查，不能仅依赖本函数防御 SSRF。
func validateURL(value *url.URL) error {
	if value.Scheme != "http" && value.Scheme != "https" {
		return errors.New("subscription URL must use HTTP or HTTPS")
	}
	if value.Hostname() == "" || value.User != nil {
		return errors.New("subscription URL host is invalid")
	}
	if port := value.Port(); port != "" {
		parsed, err := strconv.ParseUint(port, 10, 16)
		if err != nil || parsed == 0 {
			return errors.New("subscription URL port is invalid")
		}
	}
	if address, err := netip.ParseAddr(value.Hostname()); err == nil && !publicAddress(address) {
		return errors.New("subscription URL points to a blocked address")
	}
	return nil
}

// publicAddress 判断地址是否符合订阅拨号的允许策略。
//
// IPv4-mapped IPv6 会先 Unmap，避免用 ::ffff:127.0.0.1 绕过 IPv4 回环检查。策略明确
// 拒绝无效、私有、回环、链路本地、组播和未指定地址。该函数名表达“允许作为公网
// 目标”，但它不是互联网可达性证明：未被这些 netip 分类覆盖的保留地址仍可能通过，
// 后续若安全策略要求更严格，应在此集中补充拒绝网段。
func publicAddress(address netip.Addr) bool {
	address = address.Unmap()
	if !address.IsValid() || !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() ||
		address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() || address.IsMulticast() || address.IsUnspecified() {
		return false
	}
	// IsPrivate does not include several special-use ranges that a public service
	// must never fetch: CGNAT, benchmarking, documentation/test networks, IPv4
	// special-purpose space and IPv6 documentation space.
	for _, blocked := range []netip.Prefix{
		netip.MustParsePrefix("100.64.0.0/10"),
		netip.MustParsePrefix("192.0.0.0/24"),
		netip.MustParsePrefix("192.0.2.0/24"),
		netip.MustParsePrefix("198.18.0.0/15"),
		netip.MustParsePrefix("198.51.100.0/24"),
		netip.MustParsePrefix("203.0.113.0/24"),
		netip.MustParsePrefix("2001:db8::/32"),
	} {
		if blocked.Contains(address) {
			return false
		}
	}
	return true
}
