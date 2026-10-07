package main

// Local forward proxy for headless Chromium.
//
// Chromium reads proxy settings on its own (environment variables, system
// configuration, auto-detection) and differently from Go: it fails with
// net::ERR_PROXY_CONNECTION_FAILED where the Go HTTP client works, cannot use
// proxy credentials passed on the command line, and ignores NO_PROXY quirks.
// Instead of relying on that, Chromium is pointed at this forwarder on
// 127.0.0.1, which dials out with the same proxy rules the Go client uses
// (HTTP_PROXY / HTTPS_PROXY / NO_PROXY, http(s) and socks5 proxies, basic auth).

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/http/httpproxy"
	xproxy "golang.org/x/net/proxy"
)

type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// envProxyFor returns the proxy the Go HTTP stack would use for an https
// connection to host:port, or nil for a direct connection.
func envProxyFor(hostport string) *url.URL {
	// httpproxy re-reads the environment on every call (http.ProxyFromEnvironment
	// caches it for the life of the process) and applies the same NO_PROXY and
	// loopback rules as net/http.
	u, err := httpproxy.FromEnvironment().ProxyFunc()(&url.URL{Scheme: "https", Host: hostport})
	if err != nil {
		return nil
	}
	return u
}

// redactProxyURL hides credentials for logging.
func redactProxyURL(u *url.URL) string {
	if u == nil {
		return "direct"
	}
	c := *u
	if c.User != nil {
		c.User = url.User(c.User.Username())
	}
	return c.String()
}

func proxyHostPort(u *url.URL) string {
	host := u.Host
	if u.Port() == "" {
		switch u.Scheme {
		case "https":
			host = net.JoinHostPort(u.Hostname(), "443")
		case "socks5", "socks5h":
			host = net.JoinHostPort(u.Hostname(), "1080")
		default:
			host = net.JoinHostPort(u.Hostname(), "80")
		}
	}
	return host
}

// dialViaEnvProxy opens a TCP connection to addr, going through the proxy
// configured in the environment (if any).
func dialViaEnvProxy(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	pu := envProxyFor(addr)
	if pu == nil {
		return d.DialContext(ctx, network, addr)
	}
	switch pu.Scheme {
	case "socks5", "socks5h":
		var auth *xproxy.Auth
		if pu.User != nil {
			pass, _ := pu.User.Password()
			auth = &xproxy.Auth{User: pu.User.Username(), Password: pass}
		}
		sd, err := xproxy.SOCKS5("tcp", proxyHostPort(pu), auth, d)
		if err != nil {
			return nil, err
		}
		if cd, ok := sd.(xproxy.ContextDialer); ok {
			return cd.DialContext(ctx, network, addr)
		}
		return sd.Dial(network, addr)
	case "http", "https", "":
		conn, err := d.DialContext(ctx, "tcp", proxyHostPort(pu))
		if err != nil {
			return nil, fmt.Errorf("connect to proxy %s: %w", redactProxyURL(pu), err)
		}
		if pu.Scheme == "https" {
			tc := tls.Client(conn, &tls.Config{ServerName: pu.Hostname()})
			if err := tc.HandshakeContext(ctx); err != nil {
				conn.Close()
				return nil, err
			}
			conn = tc
		}
		req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n", addr, addr)
		if pu.User != nil {
			pass, _ := pu.User.Password()
			req += "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(pu.User.Username()+":"+pass)) + "\r\n"
		}
		req += "\r\n"
		if _, err := io.WriteString(conn, req); err != nil {
			conn.Close()
			return nil, err
		}
		br := bufio.NewReader(conn)
		resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("proxy CONNECT %s: %w", addr, err)
		}
		if resp.StatusCode != http.StatusOK {
			conn.Close()
			return nil, fmt.Errorf("proxy CONNECT %s: %s", addr, resp.Status)
		}
		if br.Buffered() > 0 { // bytes the server sent right after the 200
			return &bufferedConn{Conn: conn, r: br}, nil
		}
		return conn, nil
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q", pu.Scheme)
	}
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }

// proxyForwarder is a minimal HTTP/CONNECT forward proxy bound to loopback.
type proxyForwarder struct {
	ln        net.Listener
	srv       *http.Server
	dial      dialFunc
	transport *http.Transport
	logger    *slog.Logger
	once      sync.Once
}

func startProxyForwarder(logger *slog.Logger, dial dialFunc) (*proxyForwarder, error) {
	if dial == nil {
		dial = dialViaEnvProxy
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	f := &proxyForwarder{ln: ln, dial: dial, logger: logger}
	f.transport = &http.Transport{
		Proxy:               nil, // dial already goes through the environment proxy
		DialContext:         func(ctx context.Context, n, a string) (net.Conn, error) { return dial(ctx, n, a) },
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     30 * time.Second,
	}
	f.srv = &http.Server{Handler: f, ReadHeaderTimeout: 30 * time.Second}
	go func() { _ = f.srv.Serve(ln) }()
	return f, nil
}

// URL is the value for Chromium's --proxy-server.
func (f *proxyForwarder) URL() string { return "http://" + f.ln.Addr().String() }

func (f *proxyForwarder) Close() {
	f.once.Do(func() {
		_ = f.srv.Close()
		f.transport.CloseIdleConnections()
	})
}

func (f *proxyForwarder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		f.handleConnect(w, r)
		return
	}
	f.handlePlain(w, r)
}

// Chromium makes background calls of its own (sign-in, search suggestions,
// component updates). They are useless here and just add noise and delays, so
// the forwarder refuses them without dialing.
var blockedBrowserHosts = []string{"google.com", "googleapis.com", "gstatic.com", "googleusercontent.com", "doubleclick.net", "gvt1.com", "gvt2.com"}

func isBlockedBrowserHost(hostport string) bool {
	host := strings.ToLower(hostport)
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = strings.ToLower(h)
	}
	for _, b := range blockedBrowserHosts {
		if host == b || strings.HasSuffix(host, "."+b) {
			return true
		}
	}
	return false
}

func (f *proxyForwarder) handleConnect(w http.ResponseWriter, r *http.Request) {
	if isBlockedBrowserHost(r.Host) {
		http.Error(w, "blocked", http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	upstream, err := f.dial(ctx, "tcp", r.Host)
	if err != nil {
		if f.logger != nil {
			f.logger.Warn("browser proxy: connect failed", "target", r.Host, "error", err)
		}
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		upstream.Close()
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	client, buf, err := hj.Hijack()
	if err != nil {
		upstream.Close()
		return
	}
	_, _ = client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	go func() {
		defer client.Close()
		defer upstream.Close()
		done := make(chan struct{}, 2)
		go func() {
			if buf.Reader.Buffered() > 0 {
				_, _ = io.CopyN(upstream, buf, int64(buf.Reader.Buffered()))
			}
			_, _ = io.Copy(upstream, client)
			done <- struct{}{}
		}()
		go func() { _, _ = io.Copy(client, upstream); done <- struct{}{} }()
		<-done
	}()
}

var hopHeaders = []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

func (f *proxyForwarder) handlePlain(w http.ResponseWriter, r *http.Request) {
	if !r.URL.IsAbs() {
		http.Error(w, "proxy request must use an absolute URL", http.StatusBadRequest)
		return
	}
	if isBlockedBrowserHost(r.URL.Host) {
		http.Error(w, "blocked", http.StatusForbidden)
		return
	}
	out := r.Clone(r.Context())
	out.RequestURI = ""
	for _, h := range hopHeaders {
		out.Header.Del(h)
	}
	resp, err := f.transport.RoundTrip(out)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for _, h := range hopHeaders {
		resp.Header.Del(h)
	}
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	fl, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			_, _ = w.Write(buf[:n])
			if fl != nil {
				fl.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// describeBrowserProxy explains which route Chromium will use, for the startup log.
func describeBrowserProxy(configured string, base *url.URL) string {
	if strings.EqualFold(configured, "direct") {
		return "direct (BROWSER_PROXY=direct)"
	}
	if configured != "" {
		return "BROWSER_PROXY=" + configured
	}
	host := base.Host
	if base.Port() == "" {
		host = net.JoinHostPort(base.Hostname(), map[string]string{"http": "80"}[base.Scheme])
		if base.Scheme == "https" {
			host = net.JoinHostPort(base.Hostname(), "443")
		}
	}
	return "local forwarder -> " + redactProxyURL(envProxyFor(host))
}
