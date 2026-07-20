package model

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"
)

// ProtocolVersion 是 WebSocket 应用层协议版本。客户端和服务端只有在版本一致时才解析
// 业务载荷；修改不兼容的 Envelope 或 Payload 结构时应递增该值。
const ProtocolVersion = 1

// ProxySpec 是服务端下发给客户端的一条可执行代理配置。
//
// Server 和 Port 仅供脱敏展示；实际拨号参数以 Outbound 为准。Outbound 可能包含密码、
// UUID、私钥等凭据，不应写入普通日志或测速结果。
type ProxySpec struct {
	// ID 在一次导入和任务中唯一，用于关联进度与结果。
	ID string `json:"id"`
	// Name 是订阅或 URI 中的节点显示名称。
	Name string `json:"name"`
	// Protocol 是标准化后的协议名称，例如 vless、shadowsocks 或 hysteria2。
	Protocol string `json:"protocol"`
	// Server 是代理服务器主机名或 IP 地址的原始值。
	Server string `json:"server"`
	// Port 是代理服务器端口。
	Port uint16 `json:"port"`
	// Outbound 是可直接交给 sing-box 解析的单条出站 JSON，tag 必须为 proxy。
	Outbound json.RawMessage `json:"outbound"`
}

// ImportError 描述订阅或批量文本中某一条输入的解析失败，不影响其他合法节点导入。
type ImportError struct {
	// Line 是从 1 开始的输入行号。
	Line int `json:"line"`
	// Input 只保留协议 scheme 和 [redacted] 占位符，绝不包含原始分享链接。
	Input string `json:"input"`
	// Error 是适合展示给管理员的解析错误文本。
	Error string `json:"error"`
}

// ImportResult 汇总一次批量导入中成功解析的代理和逐行错误。
type ImportResult struct {
	// Proxies 是可进入测速任务的合法代理列表。
	Proxies []ProxySpec `json:"proxies"`
	// Errors 仅包含失败项；omitempty 让全量成功响应保持简洁。
	Errors []ImportError `json:"errors,omitempty"`
}

// Assignment 是服务端通过 task.assign 下发给某个客户端的完整测速任务。
type Assignment struct {
	// TaskID 是任务全局标识，必须与外层 wire.Envelope.TaskID 一致。
	TaskID string `json:"task_id"`
	// Proxies 是需要依次测试的代理；其中 Outbound 含实际连接凭据。
	Proxies []ProxySpec `json:"proxies"`
	// CandidateCount 是每个代理最多进行延迟探测的 Speedtest.net 候选节点数。
	CandidateCount int `json:"candidate_count"`
	// TopN 是按延迟升序筛选后实际进行下载、上传测速的节点数。
	TopN int `json:"top_n"`
	// Threads 是 speedtest-go 单次测速允许使用的最大并发连接数，当前有效范围为 1..32。
	Threads int `json:"threads"`
	// TimeoutSeconds 是客户端执行整个 Assignment 的总截止时间，0 由客户端回退为 10 分钟。
	TimeoutSeconds int `json:"timeout_seconds"`
}

// Progress 是客户端在任务执行期间发送的瞬时状态，不作为最终测速数据持久化依据。
type Progress struct {
	// TaskID 标识进度所属任务。
	TaskID string `json:"task_id"`
	// ProxyID 标识当前代理；尚未进入具体代理阶段时可为空。
	ProxyID string `json:"proxy_id,omitempty"`
	// ProxyName 是便于界面直接显示的代理名称。
	ProxyName string `json:"proxy_name,omitempty"`
	// Phase 是稳定的可读阶段名称，例如初始化代理、延迟检测、下载测速或上传测速。
	Phase string `json:"phase"`
	// Message 是阶段补充信息，通常为协议名或测速节点名称。
	Message string `json:"message,omitempty"`
	// Current 是当前阶段已进入的序号，通常从 1 开始，初始化阶段可为 0。
	Current int `json:"current,omitempty"`
	// Total 是当前阶段预期处理的总项数，与 Current 共同计算界面进度。
	Total int `json:"total,omitempty"`
	// RateBPS 是下载或上传的实时速率，单位为 bit/s；非传输阶段省略。
	RateBPS float64 `json:"rate_bps,omitempty"`
}

// SpeedResult 是一个代理通过一个 Speedtest.net 节点得到的最终结果。
//
// 如果代理初始化或候选节点获取失败，也会生成仅包含代理信息和 Error 的结果。下载或
// 上传部分失败时，成功获得的延迟、抖动及另一方向速率仍会保留。
type SpeedResult struct {
	// TaskID 标识结果所属任务。
	TaskID string `json:"task_id"`
	// ClientID 由服务端根据当前认证连接补充，客户端执行器无需自行填写。
	ClientID string `json:"client_id,omitempty"`
	// ClientName 是查询结果时关联得到的客户端展示名称。
	ClientName string `json:"client_name,omitempty"`
	// ProxyID 关联 Assignment 中的代理。
	ProxyID string `json:"proxy_id"`
	// ProxyName 是代理显示名称的任务快照，避免订阅后续变化影响历史结果。
	ProxyName string `json:"proxy_name"`
	// Protocol 是被测代理协议。
	Protocol string `json:"protocol"`
	// MaskedAddress 是仅供展示的脱敏代理地址，不能用于实际拨号。
	MaskedAddress string `json:"masked_address"`
	// SpeedServerID 由 Client 上报原始 Speedtest.net ID，Server 落库前替换为稳定摘要。
	SpeedServerID string `json:"speed_server_id,omitempty"`
	// SpeedServerName 是测速节点所在位置或名称。
	SpeedServerName string `json:"speed_server_name,omitempty"`
	// SpeedServerHost 是 speedtest-go 返回的测速节点主机地址；Server 不持久化该字段。
	SpeedServerHost string `json:"speed_server_host,omitempty"`
	// Country 是测速节点国家或地区。
	Country string `json:"country,omitempty"`
	// Sponsor 是测速节点运营方。
	Sponsor string `json:"sponsor,omitempty"`
	// LatencyMS 是 HTTP Ping 延迟，单位为毫秒。
	LatencyMS float64 `json:"latency_ms,omitempty"`
	// JitterMS 是延迟抖动，单位为毫秒。
	JitterMS float64 `json:"jitter_ms,omitempty"`
	// DownloadBPS 是下载速率，单位为 bit/s。
	DownloadBPS float64 `json:"download_bps,omitempty"`
	// UploadBPS 是上传速率，单位为 bit/s。
	UploadBPS float64 `json:"upload_bps,omitempty"`
	// DurationMS 是该测速节点下载与上传阶段的总墙钟耗时，单位为毫秒。
	DurationMS int64 `json:"duration_ms,omitempty"`
	// Error 保存代理级或传输阶段错误；为空表示该条结果完整成功。
	Error string `json:"error,omitempty"`
	// CreatedAt 是客户端生成结果时的 UTC RFC3339Nano 时间戳。
	CreatedAt string `json:"created_at"`
}

// Hello 是 WebSocket Upgrade 成功后客户端发送的第一条应用层消息。
type Hello struct {
	// Name 是管理员可见的客户端名称。
	Name string `json:"name"`
	// Version 是客户端二进制构建版本，不等同于 ProtocolVersion。
	Version string `json:"version"`
	// OS 是客户端构建时的 runtime.GOOS。
	OS string `json:"os"`
	// Arch 是客户端构建时的 runtime.GOARCH。
	Arch string `json:"arch"`
	// Labels 是部署者提供的地区、线路等自由键值元数据。
	Labels map[string]string `json:"labels,omitempty"`
}

// Welcome 是服务端确认 Hello 后返回的连接身份。
type Welcome struct {
	// ClientID 是服务端令牌记录对应的稳定客户端 ID。
	ClientID string `json:"client_id"`
}

// Ack 表示客户端已接收并开始处理指定任务，或作为成功终态的任务引用载荷。
type Ack struct {
	// TaskID 必须与外层 Envelope.TaskID 一致。
	TaskID string `json:"task_id"`
}

// Failure 是 task.failed 的载荷，描述任务未能完整结束的原因。
type Failure struct {
	// TaskID 标识失败任务。
	TaskID string `json:"task_id"`
	// Error 通常来自任务超时、服务端取消或上层 context 取消。
	Error string `json:"error"`
}

// NewID 生成 128 位随机 ID，并以 32 个小写十六进制字符返回。
//
// 系统随机源不可用时会退化为 UTC RFC3339Nano 时间戳的十六进制编码。退化值仍适合
// 日志和关联用途，但不具备密码学随机性，也不应被当作认证凭据。
func NewID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return hex.EncodeToString([]byte(time.Now().UTC().Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(value[:])
}

// MaskAddress 完全隐藏代理主机地址，只保留端口供结果排查和协议核对。
//
// 公共服务不能把 IPv4 前缀、IPv6 前缀或域名后缀当成“脱敏后可公开”的信息；这些值
// 仍可能定位节点。因此 host 参数只用于保持统一调用接口，不参与返回值。
func MaskAddress(host string, port uint16) string {
	_ = host
	return "[redacted]:" + portString(port)
}

// portString 将 uint16 端口转换为十进制文本；端口 0 明确返回 "0"。
func portString(port uint16) string {
	if port == 0 {
		return "0"
	}
	var buf [5]byte
	i := len(buf)
	for port > 0 {
		i--
		buf[i] = byte('0' + port%10)
		port /= 10
	}
	return string(buf[i:])
}
