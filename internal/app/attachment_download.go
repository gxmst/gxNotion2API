package app

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

var errLocalAttachmentPath = errors.New("server file paths are not allowed as attachments; send inline data or a public HTTP(S) URL")

// attachmentProxyError marks an attachment download that failed inside the
// configured outbound proxy rather than at the client-supplied URL. It is an
// infrastructure problem, so unlike other download failures it is not reported
// back to the client as invalid input.
type attachmentProxyError struct{ err error }

func (e *attachmentProxyError) Error() string { return e.err.Error() }
func (e *attachmentProxyError) Unwrap() error { return e.err }

func isAttachmentProxyError(err error) bool {
	var target *attachmentProxyError
	return errors.As(err, &target)
}

func parseAttachmentURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil || u.Hostname() == "" || u.Opaque != "" || u.User != nil ||
		(u.Scheme != "http" && u.Scheme != "https") || strings.Contains(u.Hostname(), "%") {
		return nil, errors.New("attachment URL must be an absolute HTTP(S) URL without credentials or an IPv6 zone")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, errors.New("invalid attachment URL port")
		}
	}
	return u, nil
}

var nonPublicAttachmentNetworks = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
}

func publicAttachmentIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	// Only native global IPv6 unicast; translation/tunnelling prefixes can
	// embed otherwise blocked IPv4 destinations.
	if ip.Is6() && !netip.MustParsePrefix("2000::/3").Contains(ip) {
		return false
	}
	for _, prefix := range nonPublicAttachmentNetworks {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

type attachmentTransport struct {
	resolver *ProxyResolver
	email    string
	lookupIP func(context.Context, string, string) ([]netip.Addr, error)
	dial     func(context.Context, string, string) (net.Conn, error)
}

func (c *NotionAIClient) attachmentHTTPClient() *http.Client {
	resolver := c.ProxyResolver
	if resolver == nil {
		resolver = NewProxyResolver(c.Config)
	}
	return &http.Client{
		Timeout:   requestTimeout(c.Config),
		Transport: &attachmentTransport{resolver: resolver, email: c.AccountEmail},
	}
}

// Each redirect goes through RoundTrip again. Resolve once per hop and dial
// only the checked IPs, including through proxies, so DNS rebinding cannot
// change the destination after validation. This client carries no Notion
// cookies and never inherits upstream TLS verification overrides.
func (t *attachmentTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u, err := parseAttachmentURL(req.URL.String())
	if err != nil {
		return nil, err
	}
	lookup := t.lookupIP
	if lookup == nil {
		lookup = net.DefaultResolver.LookupNetIP
	}
	var ips []netip.Addr
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil {
		ips = []netip.Addr{ip}
	} else {
		ips, err = lookup(req.Context(), "ip", u.Hostname())
		if err != nil {
			return nil, errors.New("cannot resolve attachment URL host")
		}
	}
	if len(ips) == 0 {
		return nil, errors.New("attachment URL host has no addresses")
	}
	for _, ip := range ips {
		if !publicAttachmentIP(ip) {
			return nil, errors.New("attachment URL must resolve only to public IP addresses")
		}
	}
	proxyURL, _, err := t.resolver.ResolveProxyForRequest(t.email, u)
	if err != nil {
		return nil, &attachmentProxyError{err: err}
	}
	port := u.Port()
	if port == "" {
		port = "443"
		if u.Scheme == "http" {
			port = "80"
		}
	}
	dial := t.dial
	if dial == nil {
		dial = (&net.Dialer{Timeout: 15 * time.Second}).DialContext
	}
	transport := &http.Transport{
		DisableKeepAlives:     true,
		TLSClientConfig:       &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var lastErr error
			for _, ip := range ips {
				target := net.JoinHostPort(ip.Unmap().String(), port)
				conn, err := dialAttachmentTarget(ctx, target, proxyURL, dial)
				if err == nil {
					return conn, nil
				}
				lastErr = err
				if ctx.Err() != nil {
					break
				}
			}
			return nil, lastErr
		},
	}
	return transport.RoundTrip(req)
}

type attachmentProxyDialer struct {
	ctx  context.Context
	dial func(context.Context, string, string) (net.Conn, error)
}

func (d attachmentProxyDialer) Dial(network, address string) (net.Conn, error) {
	return d.dial(d.ctx, network, address)
}

func (d attachmentProxyDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return d.dial(ctx, network, address)
}

func dialAttachmentTarget(ctx context.Context, target string, proxyURL *url.URL, dial func(context.Context, string, string) (net.Conn, error)) (net.Conn, error) {
	if proxyURL == nil {
		return dial(ctx, "tcp", target)
	}
	if proxyURL.Scheme == "socks5" || proxyURL.Scheme == "socks5h" {
		d, err := proxy.FromURL(proxyURL, attachmentProxyDialer{ctx: ctx, dial: dial})
		if err != nil {
			return nil, &attachmentProxyError{err: errors.New("invalid attachment SOCKS proxy")}
		}
		conn, err := d.(proxy.ContextDialer).DialContext(ctx, "tcp", target)
		if err != nil {
			return nil, &attachmentProxyError{err: errors.New("attachment SOCKS proxy connection failed")}
		}
		return conn, nil
	}
	if proxyURL.Scheme != "http" && proxyURL.Scheme != "https" {
		return nil, &attachmentProxyError{err: errors.New("unsupported attachment proxy scheme")}
	}
	port := proxyURL.Port()
	if port == "" {
		port = "80"
		if proxyURL.Scheme == "https" {
			port = "443"
		}
	}
	conn, err := dial(ctx, "tcp", net.JoinHostPort(proxyURL.Hostname(), port))
	if err != nil {
		return nil, &attachmentProxyError{err: errors.New("attachment proxy connection failed")}
	}
	// Context cancellation must also interrupt CONNECT's reads and writes.
	rawConn := conn
	stop := context.AfterFunc(ctx, func() { _ = rawConn.Close() })
	connected := false
	defer func() {
		stop()
		if !connected {
			_ = rawConn.Close()
		}
	}()
	deadline := time.Now().Add(15 * time.Second)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	_ = conn.SetDeadline(deadline)
	if proxyURL.Scheme == "https" {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: proxyURL.Hostname(), MinVersion: tls.VersionTLS12})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return nil, &attachmentProxyError{err: errors.New("attachment HTTPS proxy TLS handshake failed")}
		}
		conn = tlsConn
	}
	// Tunnel HTTP as well as HTTPS: a forward proxy would otherwise resolve
	// the original Host itself and bypass the checked destination IP.
	connect := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: target}, Host: target, Header: make(http.Header)}
	if proxyURL.User != nil {
		password, _ := proxyURL.User.Password()
		credentials := proxyURL.User.Username() + ":" + password
		connect.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(credentials)))
	}
	if err := connect.Write(conn); err != nil {
		return nil, &attachmentProxyError{err: errors.New("attachment proxy CONNECT write failed")}
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, connect)
	if err != nil {
		return nil, &attachmentProxyError{err: errors.New("attachment proxy CONNECT response failed")}
	}
	// A CONNECT the proxy answered but refused is about the destination the
	// client supplied, so it stays an ordinary (client input) failure.
	if response.StatusCode != http.StatusOK {
		_, targetPort, _ := net.SplitHostPort(target)
		if targetPort != "443" && (response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusMethodNotAllowed) {
			return nil, fmt.Errorf("attachment proxy rejected CONNECT to port %s: HTTP %d; use an HTTPS attachment URL if available, or configure the proxy to allow CONNECT to this port", targetPort, response.StatusCode)
		}
		return nil, fmt.Errorf("attachment proxy CONNECT failed: HTTP %d", response.StatusCode)
	}
	_ = conn.SetDeadline(time.Time{})
	connected = true
	return &attachmentBufferedConn{Conn: conn, reader: reader}, nil
}

type attachmentBufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *attachmentBufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }
