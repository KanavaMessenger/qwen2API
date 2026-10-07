package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testEngine(t *testing.T, base string) *browserEngine {
	t.Helper()
	path, err := findChromium(testChromiumPath)
	if err != nil {
		t.Skipf("no chromium available: %v", err)
	}
	eng, err := newBrowserEngine(browserEngineConfig{
		BaseURL: base, ExecPath: path, PoolSize: 1, HeaderTimeout: 20 * time.Second,
		WarmupDelay: 100 * time.Millisecond, Headless: true,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(eng.Close)
	return eng
}

const testChromiumPath = ""

func newMockUpstream(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var challengeLeft atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "<html><body>mock qwen</body></html>")
	})
	mux.HandleFunc("/api/echo", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"auth": r.Header.Get("Authorization"), "method": r.Method, "body": string(body),
			"ua": r.Header.Get("User-Agent"), "origin": r.Header.Get("Origin"),
		})
	})
	mux.HandleFunc("/api/sse", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			io.WriteString(w, "data: {\"n\":"+string(rune('0'+i))+",\"t\":\"привет\"}\n\n")
			fl.Flush()
			time.Sleep(150 * time.Millisecond)
		}
		io.WriteString(w, "data: [DONE]\n\n")
	})
	mux.HandleFunc("/api/captcha", func(w http.ResponseWriter, r *http.Request) {
		if challengeLeft.Add(-1) >= 0 {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, `{"data":{"url":"https://x/_____tmd_____/punish?x5secdata=abc"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	})
	mux.HandleFunc("/api/hang", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &challengeLeft
}

func doReq(t *testing.T, tr http.RoundTripper, method, url, body string) (*http.Response, error) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok123")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "should-be-ignored")
	return tr.RoundTrip(req)
}

func TestBrowserEngineJSONAndHeaders(t *testing.T) {
	srv, _ := newMockUpstream(t)
	eng := testEngine(t, srv.URL)
	tr := &browserTransport{engine: eng, fallback: http.DefaultTransport}

	resp, err := doReq(t, tr, http.MethodPost, srv.URL+"/api/echo", `{"a":"тест"}`)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var got map[string]string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("bad body %q: %v", raw, err)
	}
	if resp.StatusCode != 200 || got["auth"] != "Bearer tok123" || got["method"] != "POST" || got["body"] != `{"a":"тест"}` {
		t.Fatalf("unexpected echo: status=%d %v", resp.StatusCode, got)
	}
	if !strings.Contains(got["ua"], "Chrome") || got["ua"] == "should-be-ignored" {
		t.Fatalf("user-agent must come from the browser, got %q", got["ua"])
	}
}

func TestBrowserEngineStreaming(t *testing.T) {
	srv, _ := newMockUpstream(t)
	eng := testEngine(t, srv.URL)
	tr := &browserTransport{engine: eng, fallback: http.DefaultTransport}

	resp, err := doReq(t, tr, http.MethodPost, srv.URL+"/api/sse", "{}")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	start := time.Now()
	var lines []string
	var firstAt time.Duration
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data:") {
			if len(lines) == 0 {
				firstAt = time.Since(start)
			}
			lines = append(lines, sc.Text())
		}
	}
	if len(lines) != 4 || !strings.Contains(lines[0], "привет") || lines[3] != "data: [DONE]" {
		t.Fatalf("unexpected sse lines: %v", lines)
	}
	if time.Since(start) < 300*time.Millisecond || firstAt > 300*time.Millisecond {
		t.Fatalf("stream was not incremental: first=%s total=%s", firstAt, time.Since(start))
	}
}

func TestBrowserEngineRotatesOnWAFChallenge(t *testing.T) {
	srv, left := newMockUpstream(t)
	eng := testEngine(t, srv.URL)
	tr := &browserTransport{engine: eng, fallback: http.DefaultTransport}

	left.Store(1) // first attempt is challenged, the retry in a fresh session succeeds
	resp, err := doReq(t, tr, http.MethodPost, srv.URL+"/api/captcha", "{}")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(raw) != `{"ok":true}` {
		t.Fatalf("unexpected body after rotation: %q", raw)
	}

	left.Store(100) // permanently challenged -> explicit error, not an empty 200
	_, err = doReq(t, tr, http.MethodPost, srv.URL+"/api/captcha", "{}")
	if !errors.Is(err, errWAFChallenge) {
		t.Fatalf("expected errWAFChallenge, got %v", err)
	}
}

func TestBrowserEngineCancel(t *testing.T) {
	srv, _ := newMockUpstream(t)
	eng := testEngine(t, srv.URL)
	tr := &browserTransport{engine: eng, fallback: http.DefaultTransport}

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/api/hang", strings.NewReader("{}"))
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	_, err = io.ReadAll(resp.Body)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	resp.Body.Close()
	// Engine must stay usable afterwards.
	resp2, err := doReq(t, tr, http.MethodGet, srv.URL+"/api/echo", "")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
}

func TestIsWAFChallenge(t *testing.T) {
	if !isWAFChallenge(`..."url":"https://chat.qwen.ai:443//api/v2/chat/completions/_____tmd_____/punish?x5secdata=xga8"`) {
		t.Fatal("captcha body must be detected")
	}
	if isWAFChallenge(`data: {"choices":[]}`) || isWAFChallenge("") {
		t.Fatal("regular body must not be detected")
	}
}

func newMockQwen(t *testing.T, captcha bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "<html><body>mock</body></html>")
	})
	mux.HandleFunc("/api/v2/chats/new", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"success":true,"data":{"id":"chat-1"}}`)
	})
	mux.HandleFunc("/api/v2/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if captcha {
			io.WriteString(w, `{"data":{"url":"https://chat.qwen.ai:443//x/_____tmd_____/punish?x5secdata=xga8"}}`)
			return
		}
		for _, part := range []string{"Go ", "это ", "язык"} {
			b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": part, "phase": "answer"}}}})
			io.WriteString(w, "data: "+string(b)+"\n\n")
			w.(http.Flusher).Flush()
		}
		io.WriteString(w, "data: [DONE]\n\n")
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func withBaseURL(t *testing.T, u string) {
	t.Helper()
	old := qwenBaseURL
	qwenBaseURL = u
	t.Cleanup(func() { qwenBaseURL = old })
}

func TestQwenClientThroughBrowserEngine(t *testing.T) {
	if _, err := findChromium(testChromiumPath); err != nil {
		t.Skip("no chromium available")
	}
	srv := newMockQwen(t, false)
	withBaseURL(t, srv.URL)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := NewQwenClient(nil, Settings{UpstreamEngine: engineBrowser, BrowserPoolSize: 1, BrowserHeadless: true,
		UpstreamStreamHeaderTimeoutSeconds: 20, UpstreamStreamFirstEventTimeoutSeconds: 20, UpstreamStreamIdleTimeoutSeconds: 20}, logger)
	defer client.Close()
	if client.engine == nil {
		t.Fatal("browser engine must be active")
	}
	ctx := context.Background()
	chatID, err := client.CreateChat(ctx, "tok", "qwen3.6-plus", "t2t")
	if err != nil || chatID != "chat-1" {
		t.Fatalf("CreateChat = %q, %v", chatID, err)
	}
	var text strings.Builder
	err = client.StreamChat(ctx, "tok", chatID, map[string]any{"stream": true}, func(evt UpstreamEvent) error {
		text.WriteString(evt.Content)
		return nil
	})
	if err != nil || text.String() != "Go это язык" {
		t.Fatalf("StreamChat text=%q err=%v", text.String(), err)
	}
}

func TestStreamChatReportsWAFChallengeOverHTTP(t *testing.T) {
	srv := newMockQwen(t, true)
	withBaseURL(t, srv.URL)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := NewQwenClient(nil, Settings{UpstreamEngine: engineHTTP,
		UpstreamStreamHeaderTimeoutSeconds: 20, UpstreamStreamFirstEventTimeoutSeconds: 20, UpstreamStreamIdleTimeoutSeconds: 20}, logger)
	err := client.StreamChat(context.Background(), "tok", "chat-1", map[string]any{}, func(UpstreamEvent) error { return nil })
	if !errors.Is(err, errWAFChallenge) {
		t.Fatalf("expected errWAFChallenge instead of a silent empty answer, got %v", err)
	}
}

func TestJWTExpiryUnix(t *testing.T) {
	// header.{"exp":1791332741}.sig
	token := "eyJhbGciOiJIUzI1NiJ9.eyJleHAiOjE3OTEzMzI3NDF9.sig"
	if got := jwtExpiryUnix(token); got != 1791332741 {
		t.Fatalf("exp = %d", got)
	}
	if jwtExpiryUnix("not-a-jwt") != 0 || jwtExpiryUnix("") != 0 {
		t.Fatal("non-JWT must give 0")
	}
}

func TestRandomChatTitle(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 2000; i++ {
		title := randomChatTitle()
		if title == "" || len(title) > 60 || strings.HasPrefix(title, "api_") {
			t.Fatalf("bad title %q", title)
		}
		seen[title] = true
	}
	if len(seen) < 1500 {
		t.Fatalf("titles are not random enough: %d distinct of 2000", len(seen))
	}
}

// failingTransport makes any request that bypasses the browser fail the test.
type failingTransport struct{ t *testing.T }

func (f failingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	f.t.Errorf("request bypassed headless Chromium: %s %s", req.Method, req.URL)
	return nil, errors.New("direct request not allowed")
}

func TestAllQwenClientTrafficGoesThroughChromium(t *testing.T) {
	if _, err := findChromium(testChromiumPath); err != nil {
		t.Skip("no chromium available")
	}
	var browserHits, directHits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/favicon.ico" {
			// Real browsers always send Fetch Metadata headers and a Chrome UA;
			// Go's net/http sends neither.
			if r.Header.Get("Sec-Fetch-Mode") == "cors" && strings.Contains(r.Header.Get("User-Agent"), "Chrome") && !strings.Contains(r.Header.Get("User-Agent"), "Go-http-client") {
				browserHits.Add(1)
			} else {
				directHits.Add(1)
			}
		}
		if strings.HasSuffix(r.URL.Path, "/completions") {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"success":true,"data":{"id":"x"}}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	withBaseURL(t, srv.URL)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := NewQwenClient(nil, Settings{UpstreamEngine: engineBrowser, BrowserPoolSize: 1, BrowserHeadless: true, ChatDeleteRetryAttempts: 1,
		UpstreamStreamHeaderTimeoutSeconds: 20, UpstreamStreamFirstEventTimeoutSeconds: 20, UpstreamStreamIdleTimeoutSeconds: 20}, logger)
	defer client.Close()
	// Anything that skips the browser transport hits this and fails the test.
	client.http.Transport.(*browserTransport).fallback = failingTransport{t}

	ctx := context.Background()
	calls := 0
	do := func(err error) {
		t.Helper()
		calls++
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err := client.CreateChat(ctx, "tok", "qwen3.6-plus", "t2t")
	do(err)
	do(client.StreamChat(ctx, "tok", "x", map[string]any{}, func(UpstreamEvent) error { return nil }))
	_, _, err = client.PostChatCompletionOnce(ctx, "tok", "x", map[string]any{}, 10*time.Second)
	do(err)
	_, _, err = client.GetChatDetail(ctx, "tok", "x", 10*time.Second)
	do(err)
	_, _, err = client.GetVisionTaskStatus(ctx, "tok", "x", 10*time.Second)
	do(err)
	_, err = client.ListChats(ctx, "tok", 5)
	do(err)
	if res := client.VerifyTokenDetail(ctx, "tok"); !res.Valid {
		t.Fatalf("verify: %+v", res)
	}
	calls++
	if !client.DeleteChat(ctx, "tok", "x") {
		t.Fatal("DeleteChat failed")
	}
	calls++
	if browserHits.Load() < int32(calls) || directHits.Load() != 0 {
		t.Fatalf("browser=%d direct=%d, expected all %d calls via the browser", browserHits.Load(), directHits.Load(), calls)
	}
}

func TestBrowserEngineCrossOriginBinaryUpload(t *testing.T) {
	// "OSS": a different origin that needs CORS and receives binary PUT bodies.
	var got []byte
	var gotHdr http.Header
	oss := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*") // wildcard => credentials must be omitted
		w.Header().Set("Access-Control-Allow-Headers", "*")
		w.Header().Set("Access-Control-Allow-Methods", "PUT, GET, OPTIONS")
		if r.Method == http.MethodOptions {
			return
		}
		got, _ = io.ReadAll(r.Body)
		gotHdr = r.Header.Clone()
		w.Header().Set("Content-Type", "application/xml")
		io.WriteString(w, "<ok/>")
	}))
	t.Cleanup(oss.Close)
	srv, _ := newMockUpstream(t)
	eng := testEngine(t, srv.URL)
	client := eng.HTTPClient(30 * time.Second)

	for _, size := range []int{1000, 700 << 10} { // inline path and chunked-push path
		payload := make([]byte, size)
		for i := range payload {
			payload[i] = byte(i*7 + 13) // includes invalid UTF-8 / NUL bytes
		}
		req, _ := http.NewRequest(http.MethodPut, oss.URL+"/bucket/file.bin", bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Header.Set("x-oss-security-token", "sts")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || string(body) != "<ok/>" {
			t.Fatalf("size %d: status=%d body=%q", size, resp.StatusCode, body)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("size %d: binary body corrupted (got %d bytes)", size, len(got))
		}
		if gotHdr.Get("X-Oss-Security-Token") != "sts" || !strings.Contains(gotHdr.Get("User-Agent"), "Chrome") {
			t.Fatalf("size %d: headers %v", size, gotHdr)
		}
	}
}
