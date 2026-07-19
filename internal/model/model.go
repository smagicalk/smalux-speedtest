package model

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net"
	"strings"
	"time"
)

const ProtocolVersion = 1

type ProxySpec struct {
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Protocol string          `json:"protocol"`
	Server   string          `json:"server"`
	Port     uint16          `json:"port"`
	Outbound json.RawMessage `json:"outbound"`
}

type ImportError struct {
	Line  int    `json:"line"`
	Input string `json:"input"`
	Error string `json:"error"`
}

type ImportResult struct {
	Proxies []ProxySpec   `json:"proxies"`
	Errors  []ImportError `json:"errors,omitempty"`
}

type Assignment struct {
	TaskID         string      `json:"task_id"`
	Proxies        []ProxySpec `json:"proxies"`
	CandidateCount int         `json:"candidate_count"`
	TopN           int         `json:"top_n"`
	Threads        int         `json:"threads"`
	TimeoutSeconds int         `json:"timeout_seconds"`
}

type Progress struct {
	TaskID    string  `json:"task_id"`
	ProxyID   string  `json:"proxy_id,omitempty"`
	ProxyName string  `json:"proxy_name,omitempty"`
	Phase     string  `json:"phase"`
	Message   string  `json:"message,omitempty"`
	Current   int     `json:"current,omitempty"`
	Total     int     `json:"total,omitempty"`
	RateBPS   float64 `json:"rate_bps,omitempty"`
}

type SpeedResult struct {
	TaskID          string  `json:"task_id"`
	ClientID        string  `json:"client_id,omitempty"`
	ClientName      string  `json:"client_name,omitempty"`
	ProxyID         string  `json:"proxy_id"`
	ProxyName       string  `json:"proxy_name"`
	Protocol        string  `json:"protocol"`
	MaskedAddress   string  `json:"masked_address"`
	SpeedServerID   string  `json:"speed_server_id,omitempty"`
	SpeedServerName string  `json:"speed_server_name,omitempty"`
	SpeedServerHost string  `json:"speed_server_host,omitempty"`
	Country         string  `json:"country,omitempty"`
	Sponsor         string  `json:"sponsor,omitempty"`
	LatencyMS       float64 `json:"latency_ms,omitempty"`
	JitterMS        float64 `json:"jitter_ms,omitempty"`
	DownloadBPS     float64 `json:"download_bps,omitempty"`
	UploadBPS       float64 `json:"upload_bps,omitempty"`
	DurationMS      int64   `json:"duration_ms,omitempty"`
	Error           string  `json:"error,omitempty"`
	CreatedAt       string  `json:"created_at"`
}

type Hello struct {
	Name    string            `json:"name"`
	Version string            `json:"version"`
	OS      string            `json:"os"`
	Arch    string            `json:"arch"`
	Labels  map[string]string `json:"labels,omitempty"`
}

type Welcome struct {
	ClientID string `json:"client_id"`
}

type Ack struct {
	TaskID string `json:"task_id"`
}

type Failure struct {
	TaskID string `json:"task_id"`
	Error  string `json:"error"`
}

func NewID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return hex.EncodeToString([]byte(time.Now().UTC().Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(value[:])
}

func MaskAddress(host string, port uint16) string {
	ip := net.ParseIP(host)
	if ip4 := ip.To4(); ip4 != nil {
		return net.JoinHostPort(strings.Join([]string{byteString(ip4[0]), byteString(ip4[1]), "*", "*"}, "."), portString(port))
	}
	if ip != nil {
		return net.JoinHostPort("xxxx:xxxx::", portString(port))
	}
	parts := strings.Split(host, ".")
	if len(parts) > 2 {
		host = "*." + strings.Join(parts[len(parts)-2:], ".")
	} else if host != "" {
		host = "*" + host
	}
	return net.JoinHostPort(host, portString(port))
}

func byteString(value byte) string {
	const digits = "0123456789"
	if value >= 100 {
		return string([]byte{digits[value/100], digits[(value/10)%10], digits[value%10]})
	}
	if value >= 10 {
		return string([]byte{digits[value/10], digits[value%10]})
	}
	return string(digits[value])
}

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
