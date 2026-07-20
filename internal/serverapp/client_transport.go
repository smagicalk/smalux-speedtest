package serverapp

import (
	"net"
	"net/http"
)

// secureClientWebSocketRequest reports whether a request may receive sensitive
// task assignments. A TLS-terminating reverse proxy is supported when its backend
// connection originates on loopback; remote proxy deployments should terminate TLS
// directly at the Server or tunnel their backend connection to loopback.
func secureClientWebSocketRequest(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
