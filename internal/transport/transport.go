// Package transport builds the shared *http.Client used for all upstream Kiro
// calls, honoring VPN_PROXY_URL (HTTP/HTTPS/SOCKS5) for restricted networks.
package transport

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/proxy"

	"github.com/RobinGasi/kiro-proxy/internal/config"
)

// New builds an *http.Client with a tuned Transport. VPNProxyURL may be empty
// (direct), an http(s):// URL, a socks5:// URL, or a bare host:port. There is no
// Client.Timeout because SSE streams are long-lived; ResponseHeaderTimeout caps
// the otherwise-unbounded wait before an upstream response starts.
func New(cfg *config.Config) (*http.Client, error) {
	proxyURL := ""
	headerTimeout := 60 * time.Second
	if cfg != nil {
		proxyURL = cfg.VPNProxyURL
		if cfg.UpstreamResponseHeaderTimeout > 0 {
			headerTimeout = time.Duration(cfg.UpstreamResponseHeaderTimeout * float64(time.Second))
		}
	}
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: headerTimeout,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	}

	if proxyURL = strings.TrimSpace(proxyURL); proxyURL != "" {
		if !strings.Contains(proxyURL, "://") {
			proxyURL = "http://" + proxyURL // bare host:port defaults to http
		}
		u, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("invalid VPN_PROXY_URL %q: %w", proxyURL, err)
		}
		switch strings.ToLower(u.Scheme) {
		case "http", "https":
			tr.Proxy = http.ProxyURL(u)
		case "socks5", "socks5h":
			var auth *proxy.Auth
			if u.User != nil {
				pw, _ := u.User.Password()
				auth = &proxy.Auth{User: u.User.Username(), Password: pw}
			}
			dialer, err := proxy.SOCKS5("tcp", u.Host, auth, proxy.Direct)
			if err != nil {
				return nil, fmt.Errorf("socks5 dialer: %w", err)
			}
			if cd, ok := dialer.(proxy.ContextDialer); ok {
				tr.DialContext = cd.DialContext
			} else {
				tr.DialContext = func(_ context.Context, network, addr string) (net.Conn, error) {
					return dialer.Dial(network, addr)
				}
			}
			tr.Proxy = nil
		default:
			return nil, fmt.Errorf("unsupported VPN_PROXY_URL scheme %q", u.Scheme)
		}
	}

	return &http.Client{Transport: tr}, nil
}
