package app

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAttachmentRejectsServerFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.json")
	if err := os.WriteFile(path, []byte("private-server-value"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, reference := range []string{path, "../secret.json", "file:///etc/passwd", `\\server\share\secret.csv`} {
		_, _, err := parseAttachmentDescriptor(map[string]any{"type": "file", "path": reference, "filename": "data.csv", "content_type": "text/csv"})
		if !errors.Is(err, errLocalAttachmentPath) {
			t.Errorf("path %q accepted: %v", reference, err)
		}
	}
	client := &NotionAIClient{}
	if _, _, _, err := client.loadAttachmentData(t.Context(), InputAttachment{Path: path, ContentType: "text/csv"}); !errors.Is(err, errLocalAttachmentPath) {
		t.Fatalf("direct file read accepted: %v", err)
	}
	app := newConversationRequestTestApp(t)
	app.runPromptOverride = func(*http.Request, PromptRunRequest) (InferenceResult, error) {
		t.Error("file request dispatched")
		return InferenceResult{}, nil
	}
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, conversationTestHTTPRequest(t, "/v1/chat/completions", map[string]any{
		"messages":    []any{map[string]any{"role": "user", "content": "read attachment"}},
		"attachments": []any{map[string]any{"type": "file", "path": path, "filename": "data.csv", "content_type": "text/csv"}},
	}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("file request status=%d: %s", rec.Code, rec.Body.String())
	}
}

func TestAttachmentInlineDataStillWorks(t *testing.T) {
	data := "name,value\nexample,42\n"
	input, ok, err := parseAttachmentDescriptor(map[string]any{"type": "file", "filename": "data.csv", "file_data": "data:text/csv;base64," + base64.StdEncoding.EncodeToString([]byte(data))})
	if err != nil || !ok {
		t.Fatalf("parse inline attachment: %v", err)
	}
	got, name, mime, err := (&NotionAIClient{}).loadAttachmentData(t.Context(), input)
	if err != nil || string(got) != data || name != "data.csv" || mime != "text/csv" {
		t.Fatalf("inline attachment: %q %q %q %v", got, name, mime, err)
	}
}

func TestAttachmentRejectsNonPublicAddresses(t *testing.T) {
	for _, raw := range []string{"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.1.1", "169.254.169.254", "100.100.100.200", "0.0.0.0", "198.18.0.1", "192.0.2.1", "224.0.0.1", "240.0.0.1", "::", "::1", "::ffff:127.0.0.1", "fc00::1", "fe80::1", "64:ff9b::a00:1", "2002:7f00:1::", "2001:db8::1"} {
		if publicAttachmentIP(netip.MustParseAddr(raw)) {
			t.Errorf("nonpublic IP allowed: %s", raw)
		}
	}
	for _, raw := range []string{"93.184.216.34", "8.8.8.8", "2606:4700:4700::1111", "::ffff:8.8.8.8"} {
		if !publicAttachmentIP(netip.MustParseAddr(raw)) {
			t.Errorf("public IP rejected: %s", raw)
		}
	}
	for _, raw := range []string{"ftp://example.com/a", "file:///etc/passwd", "http://user:pass@example.com/a", "http://[fe80::1%25eth0]/a", "http://example.com:99999/a", "//example.com/a"} {
		if _, err := parseAttachmentURL(raw); err == nil {
			t.Errorf("invalid URL accepted: %s", raw)
		}
	}
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1); fmt.Fprint(w, "private") }))
	defer server.Close()
	client := newNotionAIClient(SessionInfo{}, defaultConfig(), "")
	if _, _, _, err := client.loadAttachmentData(t.Context(), InputAttachment{URL: server.URL, ContentType: "text/csv"}); err == nil || hits.Load() != 0 {
		t.Fatalf("loopback endpoint accessed: hits=%d err=%v", hits.Load(), err)
	}
}

func TestAttachmentDNSAndRedirectsArePinned(t *testing.T) {
	for _, mode := range []string{"public", "mixed", "private_redirect", "rebind_redirect"} {
		t.Run(mode, func(t *testing.T) {
			var hits, lookups, dials atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				if r.Host != "cdn.example" {
					t.Errorf("original Host lost: %q", r.Host)
				}
				switch mode {
				case "private_redirect":
					http.Redirect(w, r, "http://127.0.0.1/private", http.StatusFound)
				case "rebind_redirect":
					http.Redirect(w, r, "/next", http.StatusFound)
				default:
					fmt.Fprint(w, "downloaded-public-content")
				}
			}))
			defer server.Close()
			transport := &attachmentTransport{
				lookupIP: func(context.Context, string, string) ([]netip.Addr, error) {
					n := lookups.Add(1)
					if n > 1 {
						return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
					}
					ips := []netip.Addr{netip.MustParseAddr("93.184.216.34")}
					if mode == "mixed" {
						ips = append(ips, netip.MustParseAddr("10.0.0.1"))
					}
					return ips, nil
				},
				dial: func(ctx context.Context, network, address string) (net.Conn, error) {
					dials.Add(1)
					if address != "93.184.216.34:80" {
						t.Errorf("destination not pinned: %q", address)
					}
					return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
				},
			}
			client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
			response, err := client.Get("http://cdn.example/file.csv")
			if mode == "public" {
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				body, err := io.ReadAll(response.Body)
				if err != nil || string(body) != "downloaded-public-content" || lookups.Load() != 1 {
					t.Fatalf("download body=%q lookups=%d err=%v", body, lookups.Load(), err)
				}
			} else if err == nil {
				response.Body.Close()
				t.Fatal("unsafe destination accepted")
			}
			wantHits := int32(1)
			if mode == "mixed" {
				wantHits = 0
			}
			if hits.Load() != wantHits || dials.Load() != wantHits {
				t.Fatalf("unexpected network access: hits=%d dials=%d", hits.Load(), dials.Load())
			}
		})
	}
}

func TestAttachmentHTTPProxyTunnelsCheckedIP(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(fmt.Sprint(reject), func(t *testing.T) {
			var hits atomic.Int32
			proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				if r.Method != http.MethodConnect || r.Host != "93.184.216.34:80" {
					t.Errorf("proxy destination not pinned: %s %s", r.Method, r.Host)
				}
				if r.Header.Get("Proxy-Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("user:password")) {
					t.Error("proxy authentication lost")
				}
				if reject {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				conn, rw, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
				fmt.Fprint(rw, "HTTP/1.1 200 Connection Established\r\n\r\n")
				rw.Flush()
				get, err := http.ReadRequest(rw.Reader)
				if err != nil {
					t.Error(err)
					return
				}
				defer get.Body.Close()
				if get.Host != "cdn.example" || get.Header.Get("Proxy-Authorization") != "" || get.Header.Get("Cookie") != "" {
					t.Errorf("origin Host or credentials incorrect: %s %v", get.Host, get.Header)
				}
				fmt.Fprint(rw, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
				rw.Flush()
			}))
			defer proxyServer.Close()
			cfg := defaultConfig()
			cfg.ProxyMode = proxyModeHTTP
			cfg.ProxyURL = strings.Replace(proxyServer.URL, "http://", "http://user:password@", 1)
			client := (&NotionAIClient{Config: cfg}).attachmentHTTPClient()
			client.Transport.(*attachmentTransport).lookupIP = func(context.Context, string, string) ([]netip.Addr, error) {
				return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
			}
			response, err := client.Get("http://cdn.example/file.csv")
			if reject {
				if err == nil {
					response.Body.Close()
					t.Fatal("rejected proxy unexpectedly succeeded")
				}
				if strings.Contains(err.Error(), "password") {
					t.Fatal("proxy credentials leaked")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(response.Body)
				response.Body.Close()
				if err != nil || string(body) != "ok" {
					t.Fatalf("proxy download: %q %v", body, err)
				}
			}
			if hits.Load() != 1 {
				t.Fatalf("proxy bypassed: hits=%d", hits.Load())
			}
		})
	}
}

func TestAttachmentSOCKSProxyReceivesNumericDestination(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	seen := make(chan string, 1)
	go func() {
		defer serverConn.Close()
		_ = serverConn.SetDeadline(time.Now().Add(3 * time.Second))
		reader := bufio.NewReader(serverConn)
		header := make([]byte, 2)
		if _, err := io.ReadFull(reader, header); err != nil {
			seen <- err.Error()
			return
		}
		if _, err := io.CopyN(io.Discard, reader, int64(header[1])); err != nil {
			seen <- err.Error()
			return
		}
		serverConn.Write([]byte{5, 0})
		request := make([]byte, 10)
		if _, err := io.ReadFull(reader, request); err != nil {
			seen <- err.Error()
			return
		}
		if request[3] != 1 {
			seen <- "destination was not numeric IPv4"
			return
		}
		seen <- net.IP(request[4:8]).String()
		serverConn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	}()
	u, _ := url.Parse("socks5h://proxy.example:1080")
	conn, err := dialAttachmentTarget(t.Context(), "93.184.216.34:80", u, func(context.Context, string, string) (net.Conn, error) { return clientConn, nil })
	if err != nil {
		clientConn.Close()
		t.Fatal(err)
	}
	conn.Close()
	if got := <-seen; got != "93.184.216.34" {
		t.Fatalf("SOCKS destination=%s", got)
	}
}

func TestAttachmentDoesNotInheritInsecureUpstreamTLS(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { t.Error("unverified TLS server received HTTP request") }))
	defer server.Close()
	cfg := defaultConfig()
	cfg.UpstreamTLSServerName = "fronting.example"
	client := (&NotionAIClient{Config: cfg}).attachmentHTTPClient()
	transport := client.Transport.(*attachmentTransport)
	transport.lookupIP = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}
	transport.dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	response, err := client.Get("https://cdn.example/file.csv")
	if err == nil {
		response.Body.Close()
		t.Fatal("untrusted certificate accepted")
	}
}
