package clientapp

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	M "github.com/sagernet/sing/common/metadata"
)

func TestMinimalBoxDirectOutbound(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local sockets are unavailable: %v", err)
	}
	defer listener.Close()
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
