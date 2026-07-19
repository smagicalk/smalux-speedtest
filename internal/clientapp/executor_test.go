package clientapp

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	M "github.com/sagernet/sing/common/metadata"
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
