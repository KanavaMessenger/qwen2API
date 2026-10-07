package main

// Headless Chromium upstream engine.
//
// chat.qwen.ai sits behind Alibaba's baxia WAF. Requests sent from plain Go
// net/http lack the browser-generated anti-bot headers/cookies and are answered
// with a captcha challenge (x5secdata/punish) instead of the real SSE stream.
// This engine runs every upstream request as fetch() inside a real headless
// Chromium page that has loaded chat.qwen.ai, so the WAF scripts decorate the
// request exactly like they do for the official web client.
//
// It plugs into QwenClient as an http.RoundTripper, so the rest of the code
// (CreateChat, StreamChat, ...) is unchanged.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

const (
	engineBrowser = "browser"
	engineHTTP    = "http"

	browserBindingName = "q2aEmit"
	browserMaxRetries  = 2

	browserInlineBodyMax = 256 << 10 // bodies up to this size are sent inline
	browserBodyChunk     = 256 << 10
)

// errWAFChallenge is returned when the upstream answers with a captcha
// challenge instead of API data.
var errWAFChallenge = errors.New("upstream WAF captcha challenge (x5sec/punish): the request was blocked by Alibaba anti-bot")

// isWAFChallenge reports whether text looks like a baxia captcha/punish page.
func isWAFChallenge(text string) bool {
	if text == "" {
		return false
	}
	return strings.Contains(text, "_____tmd_____") ||
		strings.Contains(text, "x5secdata") ||
		strings.Contains(text, "/punish?")
}

// ---------------------------------------------------------------- stream queue

type browserChunk struct {
	data []byte
	err  error
	eof  bool
}

// chunkQueue is an unbounded FIFO. The CDP event goroutine must never block,
// so a bounded channel/io.Pipe is not an option.
type chunkQueue struct {
	mu     sync.Mutex
	items  []browserChunk
	notify chan struct{}
	closed bool
}

func newChunkQueue() *chunkQueue { return &chunkQueue{notify: make(chan struct{}, 1)} }

func (q *chunkQueue) wake() {
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

func (q *chunkQueue) push(c browserChunk) {
	q.mu.Lock()
	if !q.closed {
		q.items = append(q.items, c)
	}
	q.mu.Unlock()
	q.wake()
}

// unshift puts a chunk back in front of the queue.
func (q *chunkQueue) unshift(c browserChunk) {
	q.mu.Lock()
	if !q.closed {
		q.items = append([]browserChunk{c}, q.items...)
	}
	q.mu.Unlock()
	q.wake()
}

func (q *chunkQueue) shutdown() {
	q.mu.Lock()
	q.closed = true
	q.items = nil
	q.mu.Unlock()
	q.wake()
}

// pop waits for the next chunk. ok=false means the queue was shut down.
func (q *chunkQueue) pop(ctx context.Context) (browserChunk, bool, error) {
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			c := q.items[0]
			q.items = q.items[1:]
			q.mu.Unlock()
			return c, true, nil
		}
		closed := q.closed
		q.mu.Unlock()
		if closed {
			return browserChunk{}, false, nil
		}
		select {
		case <-q.notify:
		case <-ctx.Done():
			return browserChunk{}, false, ctx.Err()
		}
	}
}

// browserStream is one in-flight fetch() inside a page.
type browserStream struct {
	id     string
	sess   *browserSession
	head   chan browserHead
	queue  *chunkQueue
	closed atomic.Bool
}

type browserHead struct {
	status      int
	statusText  string
	contentType string
	headers     map[string]string
	err         error
}

func (s *browserStream) abort() {
	if s.closed.Swap(true) {
		return
	}
	s.sess.abortFetch(s.id)
	s.sess.streams.Delete(s.id)
	s.queue.shutdown()
}

// browserBody is the http.Response body of a browser stream.
type browserBody struct {
	stream *browserStream
	ctx    context.Context
	done   bool
}

func (b *browserBody) Read(p []byte) (int, error) {
	if b.done {
		return 0, io.EOF
	}
	c, ok, err := b.stream.queue.pop(b.ctx)
	if err != nil {
		return 0, err
	}
	if !ok {
		b.done = true
		return 0, io.ErrClosedPipe
	}
	if c.err != nil {
		b.done = true
		return 0, c.err
	}
	if c.eof {
		b.done = true
		return 0, io.EOF
	}
	n := copy(p, c.data)
	if n < len(c.data) {
		b.stream.queue.unshift(browserChunk{data: c.data[n:]})
	}
	return n, nil
}

func (b *browserBody) Close() error {
	b.stream.abort()
	return nil
}

// ---------------------------------------------------------------- session

// browserSession is one isolated Chromium browser context + warmed page.
type browserSession struct {
	id      int
	engine  *browserEngine
	ctx     context.Context
	cancel  context.CancelFunc
	streams sync.Map // id -> *browserStream
	dead    atomic.Bool
}

type browserEvent struct {
	ID          string            `json:"id"`
	Kind        string            `json:"kind"` // head | chunk | end | error
	Status      int               `json:"status"`
	StatusText  string            `json:"statusText"`
	ContentType string            `json:"contentType"`
	Headers     map[string]string `json:"headers"`
	Data        string            `json:"data"`
	B64         bool              `json:"b64"`
	Error       string            `json:"error"`
}

// fetchScript installs (idempotently) a bridge that runs fetch() in the page and
// reports head/chunks/end through the q2aEmit binding.
const fetchScript = `(() => {
  if (window.__q2a) return true;
  const ctrls = {};
  const emit = (o) => { try { window.` + browserBindingName + `(JSON.stringify(o)); } catch (e) {} };
  const isText = (ct) => /^(text\/|application\/(json|xml|javascript|x-ndjson))|\+json|event-stream/i.test(ct || '');
  const bodies = {};
  const b64ToBytes = (b64) => {
    const bin = atob(b64);
    const out = new Uint8Array(bin.length);
    for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
    return out;
  };
  window.__q2a = {
    abort(id) { const c = ctrls[id]; if (c) { try { c.abort(); } catch (e) {} } delete bodies[id]; },
    // Large request bodies are streamed in chunks to stay below CDP message limits.
    push(id, b64) { (bodies[id] = bodies[id] || []).push(b64ToBytes(b64)); },
    async run(id, url, init) {
      const ctrl = new AbortController();
      ctrls[id] = ctrl;
      init.signal = ctrl.signal;
      let crossOrigin = false;
      try { crossOrigin = new URL(url, location.href).origin !== location.origin; } catch (e) {}
      // Wildcard CORS (e.g. object storage) is incompatible with credentials.
      init.credentials = crossOrigin ? 'omit' : 'include';
      try {
        if (init.bodyPushed) {
          init.body = new Blob(bodies[id] || []);
          delete bodies[id];
        } else if (init.bodyB64 !== undefined) {
          init.body = b64ToBytes(init.bodyB64);
        }
      } catch (e) {
        emit({id, kind: 'error', error: 'bad request body: ' + String((e && e.message) || e)});
        delete ctrls[id];
        return;
      }
      delete init.bodyPushed;
      delete init.bodyB64;
      try {
        const resp = await fetch(url, init);
        const headers = {};
        resp.headers.forEach((v, k) => { headers[k] = v; });
        const ct = resp.headers.get('content-type') || '';
        emit({id, kind: 'head', status: resp.status, statusText: resp.statusText, contentType: ct, headers});
        if (!resp.body) { emit({id, kind: 'end'}); return; }
        const reader = resp.body.getReader();
        const text = isText(ct);
        const dec = new TextDecoder('utf-8');
        for (;;) {
          const {done, value} = await reader.read();
          if (done) break;
          if (text) {
            const s = dec.decode(value, {stream: true});
            if (s) emit({id, kind: 'chunk', data: s});
          } else {
            let bin = '';
            for (let i = 0; i < value.length; i += 0x8000) bin += String.fromCharCode.apply(null, value.subarray(i, i + 0x8000));
            emit({id, kind: 'chunk', data: btoa(bin), b64: true});
          }
        }
        if (text) { const s = dec.decode(); if (s) emit({id, kind: 'chunk', data: s}); }
        emit({id, kind: 'end'});
      } catch (e) {
        emit({id, kind: 'error', error: String((e && e.message) || e)});
      } finally {
        delete ctrls[id];
      }
    },
  };
  return true;
})()`

func (s *browserSession) handleBinding(payload string) {
	var ev browserEvent
	if err := json.Unmarshal([]byte(payload), &ev); err != nil {
		return
	}
	v, ok := s.streams.Load(ev.ID)
	if !ok {
		return
	}
	st := v.(*browserStream)
	switch ev.Kind {
	case "head":
		select {
		case st.head <- browserHead{status: ev.Status, statusText: ev.StatusText, contentType: ev.ContentType, headers: ev.Headers}:
		default:
		}
	case "chunk":
		data := []byte(ev.Data)
		if ev.B64 {
			decoded, err := base64.StdEncoding.DecodeString(ev.Data)
			if err != nil {
				st.queue.push(browserChunk{err: err})
				return
			}
			data = decoded
		}
		st.queue.push(browserChunk{data: data})
	case "end":
		st.queue.push(browserChunk{eof: true})
		s.streams.Delete(ev.ID)
	case "error":
		err := errors.New("browser fetch failed: " + ev.Error)
		select {
		case st.head <- browserHead{err: err}:
		default:
		}
		st.queue.push(browserChunk{err: err})
		s.streams.Delete(ev.ID)
	}
}

func (s *browserSession) abortFetch(id string) {
	if s.dead.Load() {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, 3*time.Second)
	defer cancel()
	_, _ = chromedp.Run(ctx, chromedp.Evaluate[chromedp.Void](fmt.Sprintf("window.__q2a && window.__q2a.abort(%q)", id)))
}

func (s *browserSession) close() {
	if s.dead.Swap(true) {
		return
	}
	s.streams.Range(func(_, v any) bool {
		st := v.(*browserStream)
		err := errors.New("browser session closed")
		st.queue.push(browserChunk{err: err})
		select {
		case st.head <- browserHead{err: err}:
		default:
		}
		return true
	})
	s.cancel()
}

// start sends a request through the page and returns once the response head
// has been received.
func (s *browserSession) start(ctx context.Context, id, rawURL, method string, headers map[string]string, body []byte) (*browserStream, browserHead, error) {
	st := &browserStream{id: id, sess: s, head: make(chan browserHead, 1), queue: newChunkQueue()}
	s.streams.Store(id, st)
	init := map[string]any{"method": method, "headers": headers}
	idJSON, _ := json.Marshal(id)
	urlJSON, _ := json.Marshal(rawURL)
	if len(body) > 0 && method != http.MethodGet && method != http.MethodHead {
		if len(body) <= browserInlineBodyMax {
			init["bodyB64"] = base64.StdEncoding.EncodeToString(body)
		} else {
			// Stream big bodies (file uploads) into the page in chunks.
			for off := 0; off < len(body); off += browserBodyChunk {
				end := min(off+browserBodyChunk, len(body))
				chunk := fmt.Sprintf("%s; window.__q2a.push(%s, %q); true", fetchScript, idJSON, base64.StdEncoding.EncodeToString(body[off:end]))
				pushCtx, cancelPush := context.WithTimeout(s.ctx, 30*time.Second)
				_, err := chromedp.Run(pushCtx, chromedp.Evaluate[chromedp.Void](chunk))
				cancelPush()
				if err != nil {
					s.streams.Delete(id)
					st.queue.shutdown()
					return nil, browserHead{}, fmt.Errorf("browser body upload failed: %w", err)
				}
			}
			init["bodyPushed"] = true
		}
	}
	initJSON, _ := json.Marshal(init)
	// Re-install the bridge on every call: it is lost if the page navigated.
	script := fmt.Sprintf("%s; window.__q2a.run(%s, %s, %s); true", fetchScript, idJSON, urlJSON, initJSON)
	runCtx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	_, err := chromedp.Run(runCtx, chromedp.Evaluate[chromedp.Void](script))
	cancel()
	if err != nil {
		s.streams.Delete(id)
		st.queue.shutdown()
		return nil, browserHead{}, fmt.Errorf("browser evaluate failed: %w", err)
	}
	var timeout <-chan time.Time
	if d := s.engine.headerTimeout; d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		timeout = t.C
	}
	select {
	case head := <-st.head:
		if head.err != nil {
			st.abort()
			return nil, head, head.err
		}
		return st, head, nil
	case <-timeout:
		st.abort()
		return nil, browserHead{}, fmt.Errorf("browser fetch: no response headers within %s", s.engine.headerTimeout)
	case <-ctx.Done():
		st.abort()
		return nil, browserHead{}, ctx.Err()
	case <-s.ctx.Done():
		st.abort()
		return nil, browserHead{}, errors.New("browser session closed")
	}
}

// ---------------------------------------------------------------- engine

type browserEngineConfig struct {
	BaseURL       string
	ExecPath      string
	PoolSize      int
	HeaderTimeout time.Duration
	WarmupDelay   time.Duration
	Proxy         string
	ProxyDial     dialFunc                       // test hook: how the local forwarder dials out
	ExtraOptions  []chromedp.ExecAllocatorOption // test hook
	UserAgent     string
	Headless      bool
}

type browserEngine struct {
	cfg           browserEngineConfig
	baseURL       *url.URL
	headerTimeout time.Duration
	logger        *slog.Logger

	mu        sync.Mutex
	alloc     context.Context
	cancelFn  context.CancelFunc
	forwarder *proxyForwarder
	sessions  []*browserSession
	rr        int
	seq       atomic.Int64
	closed    bool
}

func newBrowserEngine(cfg browserEngineConfig, logger *slog.Logger) (*browserEngine, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("invalid upstream base url %q", cfg.BaseURL)
	}
	if cfg.PoolSize <= 0 {
		cfg.PoolSize = 1
	}
	if cfg.HeaderTimeout <= 0 {
		cfg.HeaderTimeout = 120 * time.Second
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
	}
	return &browserEngine{cfg: cfg, baseURL: u, headerTimeout: cfg.HeaderTimeout, logger: logger}, nil
}

// findChromium locates a Chromium/Chrome binary: explicit config, Playwright's
// browser cache (what the Docker image installs), then well-known names.
func findChromium(explicit string) (string, error) {
	if p := strings.TrimSpace(explicit); p != "" {
		if _, err := os.Stat(p); err != nil {
			return "", fmt.Errorf("configured browser %q not found: %w", p, err)
		}
		return p, nil
	}
	roots := []string{os.Getenv("PLAYWRIGHT_BROWSERS_PATH"), "/ms-playwright", "/opt/pw-browsers"}
	if home, err := os.UserHomeDir(); err == nil {
		roots = append(roots, filepath.Join(home, ".cache", "ms-playwright"))
	}
	for _, root := range roots {
		if root == "" {
			continue
		}
		matches, _ := filepath.Glob(filepath.Join(root, "chromium-*", "chrome-linux*", "chrome"))
		sort.Strings(matches)
		for i := len(matches) - 1; i >= 0; i-- {
			if st, err := os.Stat(matches[i]); err == nil && !st.IsDir() {
				return matches[i], nil
			}
		}
	}
	for _, name := range []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable", "chrome"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", errors.New("no Chromium binary found; set BROWSER_PATH or install chromium (e.g. `qwen2api --install-browsers`)")
}

func (e *browserEngine) ensureBrowserLocked() error {
	if e.alloc != nil {
		return nil
	}
	path, err := findChromium(e.cfg.ExecPath)
	if err != nil {
		return err
	}
	opts := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	opts = append(opts,
		chromedp.ExecPath(path),
		chromedp.Flag("headless", e.cfg.Headless),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("lang", "en-US"),
		chromedp.UserAgent(e.cfg.UserAgent),
		chromedp.WindowSize(1365, 768),
	)
	opts = append(opts, e.cfg.ExtraOptions...)
	// Never let Chromium discover proxies by itself (that is what fails with
	// ERR_PROXY_CONNECTION_FAILED); either use the explicit BROWSER_PROXY, or a
	// local forwarder that dials with Go's proxy rules.
	switch {
	case strings.EqualFold(e.cfg.Proxy, "direct"):
		opts = append(opts, chromedp.Flag("no-proxy-server", true))
	case e.cfg.Proxy != "":
		opts = append(opts, chromedp.ProxyServer(e.cfg.Proxy))
	default:
		fwd, err := startProxyForwarder(e.logger, e.cfg.ProxyDial)
		if err != nil {
			return fmt.Errorf("start proxy forwarder: %w", err)
		}
		e.forwarder = fwd
		opts = append(opts, chromedp.ProxyServer(fwd.URL()))
	}
	alloc, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
	// Root context owns the browser process; per-session contexts derive from
	// it (WithNewBrowserContext requires an already initialised browser).
	root, cancelRoot := chromedp.NewContext(alloc)
	if err := chromedp.Do(root); err != nil {
		cancelRoot()
		cancelAlloc()
		return fmt.Errorf("browser launch failed: %w", err)
	}
	e.alloc = root
	e.cancelFn = func() { cancelRoot(); cancelAlloc() }
	if e.logger != nil {
		e.logger.Info("headless chromium engine starting", "browser", path, "pool_size", e.cfg.PoolSize, "base_url", e.cfg.BaseURL,
			"network", describeBrowserProxy(e.cfg.Proxy, e.baseURL))
	}
	return nil
}

// newSession launches an isolated browser context and warms a page on the
// upstream origin so the WAF scripts are loaded.
func (e *browserEngine) newSession(id int) (*browserSession, error) {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil, errors.New("browser engine closed")
	}
	if err := e.ensureBrowserLocked(); err != nil {
		e.mu.Unlock()
		return nil, err
	}
	alloc := e.alloc
	e.mu.Unlock()

	// The first Run on a context starts the browser tab, so it must run on the
	// long-lived ctx (no timeout) before any derived timeout context is used.
	ctx, cancel := chromedp.NewContext(alloc, chromedp.WithNewBrowserContext())
	if err := chromedp.Do(ctx); err != nil {
		cancel()
		return nil, fmt.Errorf("browser start failed: %w", err)
	}
	sess := &browserSession{id: id, engine: e, ctx: ctx, cancel: cancel}
	// Subscribe before navigating so no binding call is lost. The event queue
	// preserves order, which matters for streamed chunks.
	events := chromedp.Events(ctx, runtime.BindingCalled)
	go func() {
		for ev, err := range events {
			if err != nil {
				return
			}
			if ev.Name == browserBindingName {
				sess.handleBinding(ev.Payload)
			}
		}
	}()
	warmCtx, warmCancel := context.WithTimeout(ctx, 60*time.Second)
	defer warmCancel()
	start := time.Now()
	err := chromedp.Do(warmCtx,
		chromedp.Func(func(ctx context.Context, t *chromedp.Target) error {
			_, err := cdp.Call(ctx, t, runtime.AddBinding, runtime.AddBindingParams{Name: browserBindingName})
			return err
		}),
		chromedp.Navigate(e.baseURL.String()),
	)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("browser warm-up navigation failed: %w", err)
	}
	if e.cfg.WarmupDelay > 0 {
		select {
		case <-time.After(e.cfg.WarmupDelay):
		case <-warmCtx.Done():
		}
	}
	if _, err := chromedp.Run(warmCtx, chromedp.Evaluate[chromedp.Void](fetchScript)); err != nil {
		cancel()
		return nil, fmt.Errorf("browser fetch bridge install failed: %w", err)
	}
	if e.logger != nil {
		e.logger.Info("headless chromium session ready", "session", id, "warmup_ms", time.Since(start).Milliseconds())
	}
	return sess, nil
}

// acquire returns a live session, creating the pool lazily.
func (e *browserEngine) acquire() (*browserSession, error) {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil, errors.New("browser engine closed")
	}
	live := e.sessions[:0]
	for _, s := range e.sessions {
		if !s.dead.Load() && s.ctx.Err() == nil {
			live = append(live, s)
		}
	}
	e.sessions = live
	if len(e.sessions) >= e.cfg.PoolSize {
		e.rr = (e.rr + 1) % len(e.sessions)
		s := e.sessions[e.rr]
		e.mu.Unlock()
		return s, nil
	}
	e.mu.Unlock()

	sess, err := e.newSession(int(e.seq.Add(1)))
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		sess.close()
		return nil, errors.New("browser engine closed")
	}
	e.sessions = append(e.sessions, sess)
	return sess, nil
}

// rotate discards a session (fresh cookies/fingerprint on the next request).
func (e *browserEngine) rotate(s *browserSession) {
	s.close()
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, x := range e.sessions {
		if x == s {
			e.sessions = append(e.sessions[:i], e.sessions[i+1:]...)
			break
		}
	}
}

// Prewarm starts the browser in the background so the first request is fast.
func (e *browserEngine) Prewarm() {
	if _, err := e.acquire(); err != nil && e.logger != nil {
		e.logger.Warn("headless chromium prewarm failed", "error", err)
	}
}

func (e *browserEngine) Close() {
	e.mu.Lock()
	e.closed = true
	sessions := e.sessions
	e.sessions = nil
	cancel := e.cancelFn
	fwd := e.forwarder
	e.mu.Unlock()
	for _, s := range sessions {
		s.close()
	}
	if cancel != nil {
		cancel()
	}
	if fwd != nil {
		fwd.Close()
	}
}

// Status is exposed to the admin API.
func (e *browserEngine) Status() map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	return map[string]any{
		"engine":    engineBrowser,
		"sessions":  len(e.sessions),
		"pool_size": e.cfg.PoolSize,
		"base_url":  e.cfg.BaseURL,
		"running":   e.alloc != nil && !e.closed,
	}
}

// forbiddenFetchHeaders can't be set from page JavaScript; the browser fills
// them in itself (that is the point of using a real browser).
var forbiddenFetchHeaders = map[string]bool{
	"host": true, "user-agent": true, "origin": true, "referer": true, "connection": true,
	"content-length": true, "cookie": true, "accept-encoding": true, "keep-alive": true,
	"te": true, "upgrade": true, "via": true, "dnt": true, "date": true, "expect": true,
}

func fetchHeaders(h http.Header) map[string]string {
	out := map[string]string{}
	for k, v := range h {
		lk := strings.ToLower(k)
		if forbiddenFetchHeaders[lk] || strings.HasPrefix(lk, "sec-") || strings.HasPrefix(lk, "proxy-") || len(v) == 0 {
			continue
		}
		out[k] = strings.Join(v, ", ")
	}
	return out
}

// browserTransport adapts the engine to http.RoundTripper.
type browserTransport struct {
	engine   *browserEngine
	fallback http.RoundTripper
	// crossOrigin sends requests to other hosts (e.g. object storage) through
	// the page as well; the target must allow the page origin via CORS.
	crossOrigin bool
}

// HTTPClient returns a client that executes every request through the browser,
// including cross-origin hosts such as Alibaba OSS.
func (e *browserEngine) HTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Transport: &browserTransport{engine: e, fallback: http.DefaultTransport, crossOrigin: true}, Timeout: timeout}
}

func (t *browserTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !t.crossOrigin && (!strings.EqualFold(req.URL.Host, t.engine.baseURL.Host) || req.URL.Scheme != t.engine.baseURL.Scheme) {
		return t.fallback.RoundTrip(req)
	}
	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return nil, err
		}
	}
	ctx := req.Context()
	headers := fetchHeaders(req.Header)
	var lastErr error
	for attempt := 0; attempt <= browserMaxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		sess, err := t.engine.acquire()
		if err != nil {
			return nil, err
		}
		id := fmt.Sprintf("r%d", t.engine.seq.Add(1))
		st, head, err := sess.start(ctx, id, req.URL.String(), req.Method, headers, body)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = err
			if sess.dead.Load() || sess.ctx.Err() != nil {
				t.engine.rotate(sess)
			}
			continue
		}
		// Peek the first chunk: a WAF challenge arrives as a tiny 200 body.
		peeked, hasPeek := t.peekFirst(ctx, st)
		if hasPeek && isWAFChallenge(string(peeked)) {
			st.abort()
			if t.engine.logger != nil {
				t.engine.logger.Warn("WAF captcha challenge in headless browser, rotating session",
					"session", sess.id, "attempt", attempt+1, "path", req.URL.Path)
			}
			t.engine.rotate(sess)
			lastErr = errWAFChallenge
			continue
		}
		if hasPeek {
			st.queue.unshift(browserChunk{data: peeked})
		}
		resp := &http.Response{
			Status:     fmt.Sprintf("%d %s", head.status, head.statusText),
			StatusCode: head.status,
			Proto:      "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
			Header:  http.Header{},
			Request: req,
			Body:    &browserBody{stream: st, ctx: ctx},
		}
		for k, v := range head.headers {
			resp.Header.Set(k, v)
		}
		if head.contentType != "" {
			resp.Header.Set("Content-Type", head.contentType)
		}
		return resp, nil
	}
	if lastErr == nil {
		lastErr = errors.New("browser engine: request failed")
	}
	return nil, lastErr
}

// peekFirst waits briefly for the first body chunk and consumes it. ok is true
// only for a data chunk; end/error markers are put back for the body to report.
func (t *browserTransport) peekFirst(ctx context.Context, st *browserStream) (data []byte, ok bool) {
	wait := 20 * time.Second
	if t.engine.headerTimeout > 0 && t.engine.headerTimeout < wait {
		wait = t.engine.headerTimeout
	}
	pctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	c, got, err := st.queue.pop(pctx)
	if err != nil || !got {
		return nil, false
	}
	if c.err != nil || c.eof {
		st.queue.unshift(c)
		return nil, false
	}
	return bytes.Clone(c.data), true
}
