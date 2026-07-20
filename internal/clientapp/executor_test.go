package clientapp

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	M "github.com/sagernet/sing/common/metadata"
	"github.com/showwin/speedtest-go/speedtest"

	"smalux-speedtest/internal/model"
)

// TestMinimalBoxDirectOutbound 验证 minimalBoxContext 注册的基础协议足以启动 sing-box，
// 并确认 startBox 返回的 proxy outbound 确实能够完成一次 TCP 拨号。测试使用本地临时
// 监听器，不访问 Speedtest.net；受限沙箱禁止创建 socket 时主动 Skip，而不是误报失败。
func TestMinimalBoxDirectOutbound(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local sockets are unavailable: %v", err)
	}
	defer listener.Close()
	// accepted 证明连接真正到达监听端，而不只是 DialContext 返回了一个本地包装对象。
	accepted := make(chan struct{})
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr == nil {
			close(accepted)
			connection.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	instance, dialer, err := startBox(ctx, json.RawMessage(`{"type":"direct","tag":"proxy"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	connection, err := dialer.DialContext(ctx, "tcp", M.ParseSocksaddr(listener.Addr().String()))
	if err != nil {
		t.Fatal(err)
	}
	connection.Close()
	select {
	case <-accepted:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

// TestUsableLatencyAcceptsSuccessfulPing verifies the sentinel semantics used
// by speedtest-go.  A positive duration must be retained while PingTimeout
// and zero/negative values must be rejected before download/upload testing.
func TestUsableLatencyAcceptsSuccessfulPing(t *testing.T) {
	tests := []struct {
		name    string
		latency time.Duration
		want    bool
	}{
		{name: "successful latency", latency: 83 * time.Millisecond, want: true},
		{name: "speedtest timeout sentinel", latency: speedtest.PingTimeout, want: false},
		{name: "zero latency", latency: 0, want: false},
		{name: "negative latency", latency: -2 * time.Millisecond, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := &speedtest.Server{Latency: test.latency}
			if got := usableLatency(server); got != test.want {
				t.Fatalf("usableLatency(%s) = %v, want %v", test.latency, got, test.want)
			}
		})
	}
	if usableLatency(nil) {
		t.Fatal("nil server was considered usable")
	}
}

// TestExecutionErrorsDoNotExposeCause 确认 sing-box 和网络库的原始错误只能影响固定
// 类别，不能通过 SpeedResult.Error 流向 Server、数据库和导出文件。
func TestExecutionErrorsDoNotExposeCause(t *testing.T) {
	const secret = "vless://uuid:password@secret.example:443"
	message := executionResultError(executionError(errProxyInitialization, errors.New(secret)))
	if message != model.ResultErrorProxyInitialization || strings.Contains(message, secret) {
		t.Fatalf("unsafe execution result error: %q", message)
	}
	if got := transferResultError(errors.New(secret), nil); got != model.ResultErrorDownload {
		t.Fatalf("download error = %q", got)
	}
}
