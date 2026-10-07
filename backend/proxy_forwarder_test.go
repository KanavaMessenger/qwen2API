package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// fakeConnectProxy is an HTTP proxy that requires Basic auth and, after a
// successful CONNECT, tunnels to target (ignoring the requested host).
func fakeConnectProxy(t *testing.T, user, pass, target string, connects *atomic.Int32) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				req, err := http.ReadRequest(br)
				if err != nil || req.Method != http.MethodConnect {
					return
				}
				want := "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
				if req.Header.Get("Proxy-Authorization") != want {
					io.WriteString(c, "HTTP/1.1 407 Proxy Authentication Required\r\n\r\n")
					return
				}
				up, err := net.Dial("tcp", target)
				if err != nil {
					io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
					return
				}
				defer up.Close()
				connects.Add(1)
				io.WriteString(c, "HTTP/1.1 200 OK\r\n\r\n")
				go io.Copy(up, br)
				io.Copy(c, up)
			}(c)
		}
	}()
	return ln.Addr().String()
}

func TestDialViaEnvProxyAuthenticatedHTTPProxy(t *testing.T) {
	echo, _ := net.Listen("tcp", "127.0.0.1:0")
	t.Cleanup(func() { echo.Close() })
	go func() {
		c, err := echo.Accept()
		if err == nil {
			io.Copy(c, c)
		}
	}()
	var connects atomic.Int32
	proxyAddr := fakeConnectProxy(t, "alice", "s3cret", echo.Addr().String(), &connects)

	t.Setenv("HTTPS_PROXY", "http://alice:s3cret@"+proxyAddr)
	t.Setenv("https_proxy", "")
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	conn, err := dialViaEnvProxy(context.Background(), "tcp", "chat.qwen.example:443")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	io.WriteString(conn, "ping")
	buf := make([]byte, 4)
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "ping" || connects.Load() != 1 {
		t.Fatalf("tunnel via authenticated proxy failed: %q %v connects=%d", buf, err, connects.Load())
	}

	// wrong credentials are reported
	t.Setenv("HTTPS_PROXY", "http://alice:wrong@"+proxyAddr)
	if _, err := dialViaEnvProxy(context.Background(), "tcp", "chat.qwen.example:443"); err == nil || !strings.Contains(err.Error(), "407") {
		t.Fatalf("expected 407, got %v", err)
	}
	// NO_PROXY and an unset proxy mean a direct connection
	t.Setenv("NO_PROXY", "chat.qwen.example")
	if u := envProxyFor("chat.qwen.example:443"); u != nil {
		t.Fatalf("NO_PROXY must bypass the proxy, got %v", u)
	}
	t.Setenv("HTTPS_PROXY", "")
	if envProxyFor("chat.qwen.example:443") != nil {
		t.Fatal("no proxy configured must mean direct")
	}
}

// Chromium talks only to the local forwarder; the forwarder dials upstream.
// Covers both the CONNECT path (https) and the plain-HTTP forward path.
func TestBrowserEngineThroughLocalForwarder(t *testing.T) {
	if _, err := findChromium(testChromiumPath); err != nil {
		t.Skip("no chromium available")
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, tc := range []struct {
		name   string
		newSrv func(http.Handler) *httptest.Server
		scheme string
	}{
		{"https-connect", httptest.NewTLSServer, "https"},
		{"http-plain", httptest.NewServer, "http"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var dials atomic.Int32
			srv := tc.newSrv(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/" {
					w.Header().Set("Content-Type", "text/html")
					io.WriteString(w, "<html>ok</html>")
					return
				}
				io.WriteString(w, `{"host":"`+r.Host+`","ua":"`+r.Header.Get("User-Agent")+`"}`)
			}))
			t.Cleanup(srv.Close)
			base := tc.scheme + "://qwen.test:4443" // not loopback => Chromium must use the proxy
			eng, err := newBrowserEngine(browserEngineConfig{
				BaseURL: base, PoolSize: 1, HeaderTimeout: 20 * time.Second, WarmupDelay: 50 * time.Millisecond, Headless: true,
				ProxyDial: func(ctx context.Context, network, addr string) (net.Conn, error) {
					dials.Add(1)
					if addr != "qwen.test:4443" {
						t.Errorf("unexpected dial target %q", addr)
					}
					return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(strings.TrimPrefix(srv.URL, "https://"), "http://"))
				},
				ExtraOptions: []chromedp.ExecAllocatorOption{chromedp.Flag("ignore-certificate-errors", true)},
			}, logger)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(eng.Close)
			tr := &browserTransport{engine: eng, fallback: failingTransport{t}}
			req, _ := http.NewRequest(http.MethodGet, base+"/api/x", nil)
			resp, err := tr.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 || !strings.Contains(string(body), `"host":"qwen.test:4443"`) || !strings.Contains(string(body), "Chrome") {
				t.Fatalf("status=%d body=%s", resp.StatusCode, body)
			}
			if dials.Load() == 0 {
				t.Fatal("traffic did not go through the local forwarder")
			}
		})
	}
}
