package main

import (
	"bufio"
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
