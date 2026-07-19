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

const MaxSize = 5 << 20

type Fetcher struct {
	client *http.Client
}

func NewFetcher() *Fetcher {
	dialer := &net.Dialer{Timeout: 8 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   8 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			if err != nil {
				return nil, err
			}
			for _, address := range addresses {
				if !publicAddress(address) {
					return nil, fmt.Errorf("subscription host resolves to blocked address %s", address)
				}
			}
			if len(addresses) == 0 {
				return nil, errors.New("subscription host has no address")
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(addresses[0].String(), port))
		},
	}
	return &Fetcher{client: &http.Client{
		Transport: transport,
		Timeout:   15 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("too many subscription redirects")
			}
			return validateURL(req.URL)
		},
	}}
}

func (f *Fetcher) Fetch(ctx context.Context, rawURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", fmt.Errorf("parse subscription URL: %w", err)
	}
	if err := validateURL(parsed); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "smalux-speedtest/1")
	resp, err := f.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch subscription: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("subscription returned HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > MaxSize {
		return "", errors.New("subscription is larger than 5 MiB")
	}
	content, err := io.ReadAll(io.LimitReader(resp.Body, MaxSize+1))
	if err != nil {
		return "", err
	}
	if len(content) > MaxSize {
		return "", errors.New("subscription is larger than 5 MiB")
	}
	return string(content), nil
}

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

func publicAddress(address netip.Addr) bool {
	address = address.Unmap()
	return address.IsValid() && !address.IsPrivate() && !address.IsLoopback() && !address.IsLinkLocalUnicast() &&
		!address.IsLinkLocalMulticast() && !address.IsMulticast() && !address.IsUnspecified()
}
