//go:build linux || darwin

package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTermRingWrapsAndKeepsOffsets(t *testing.T) {
	s := &termSession{ring: make([]byte, 8)}
	s.write([]byte("abcde"))
	if got := string(s.read(0, 100)); got != "abcde" {
		t.Fatalf("read = %q", got)
	}
	s.write([]byte("fghij")) // wraps: ring holds "cdefghij", offsets 2..10
	if s.end != 10 || s.oldest() != 2 {
		t.Fatalf("end=%d oldest=%d", s.end, s.oldest())
	}
	if got := string(s.read(0, 100)); got != "cdefghij" {
		t.Fatalf("read from before oldest = %q, want the whole ring", got)
	}
	if got := string(s.read(7, 2)); got != "hi" {
		t.Fatalf("read(7,2) = %q", got)
	}
	s.write([]byte("0123456789ABC")) // longer than the ring: only its tail is kept
	if s.end != 23 || s.oldest() != 15 {
		t.Fatalf("end=%d oldest=%d", s.end, s.oldest())
	}
	if got := string(s.read(s.oldest(), 100)); got != "56789ABC" {
		t.Fatalf("ring = %q, want 56789ABC", got)
	}
}

func TestTermCursorsRoundTrip(t *testing.T) {
	c := map[int]int64{3: 10, 1: 0, 12: 99999}
	if got := formatCursors(c); got != "1:0,3:10,12:99999" {
		t.Fatalf("format = %q", got)
	}
	back := parseCursors("1:0,3:10,12:99999,junk,4:-1,x:2")
	if len(back) != 3 || back[3] != 10 || back[12] != 99999 {
		t.Fatalf("parse = %v", back)
	}
}

func TestIsLoopbackHost(t *testing.T) {
	for host, want := range map[string]bool{
		"127.0.0.1:7777": true, "localhost:7777": true, "[::1]:7777": true, "127.0.0.2": true,
		"192.168.1.5:7777": false, "0.0.0.0:7777": false, "example.com": false, "localhost.evil.com:7777": false,
	} {
		if got := isLoopbackHost(host); got != want {
			t.Errorf("isLoopbackHost(%q) = %v, want %v", host, got, want)
		}
	}
}

// termHarness wires a termManager to a test HTTP server the way Server does.
type termHarness struct {
	t   *testing.T
	m   *termManager
	srv *httptest.Server
	jar string // cookie header value after adopting the token
}

func newTermHarness(t *testing.T) *termHarness {
	t.Helper()
	t.Setenv("SHELL", "/bin/sh")
	m := newTermManager(t.TempDir(), "0", "/", "")
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if !m.Adopt(w, r) {
			w.Write([]byte("page"))
		}
	})
	mux.HandleFunc("/api/term/stream", m.handleStream)
	mux.HandleFunc("/api/term/open", m.handleOpen)
	mux.HandleFunc("/api/term/input", m.handleInput)
	mux.HandleFunc("/api/term/resize", m.handleResize)
	mux.HandleFunc("/api/term/close", m.handleClose)
	h := &termHarness{t: t, m: m, srv: httptest.NewServer(mux)}
	t.Cleanup(func() { m.Close(); h.srv.Close() })

	// Adopt the token the way the browser does on first load.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(h.srv.URL + "/?path=a.go&t=" + m.Token())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/?path=a.go" {
		t.Fatalf("adopt: status %d location %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	for _, c := range resp.Cookies() {
		if c.Name == m.cookie {
			if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
				t.Fatalf("cookie must be HttpOnly and SameSite=Strict: %+v", c)
			}
			h.jar = c.Name + "=" + c.Value
		}
	}
	if h.jar == "" {
		t.Fatal("no terminal cookie set")
	}
	return h
}

func (h *termHarness) post(path string, body any, mutate func(*http.Request)) (int, map[string]any) {
	h.t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", h.srv.URL)
	req.Header.Set("Cookie", h.jar)
	if mutate != nil {
		mutate(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// stream reads SSE events from /api/term/stream into a channel.
type termEvent struct{ id, event, data string }

func (h *termHarness) stream(since string) (<-chan termEvent, func()) {
	h.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/api/term/stream", nil)
	req.Header.Set("Cookie", h.jar)
	if since != "" {
		req.Header.Set("Last-Event-ID", since)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("stream status %d", resp.StatusCode)
	}
	ch := make(chan termEvent, 256)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		var ev termEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if ev.event != "" {
					ch <- ev
				}
				ev = termEvent{}
			case strings.HasPrefix(line, "id: "):
				ev.id = line[4:]
			case strings.HasPrefix(line, "event: "):
				ev.event = line[7:]
			case strings.HasPrefix(line, "data: "):
				ev.data = line[6:]
			}
		}
	}()
	var once sync.Once
	return ch, func() { once.Do(func() { resp.Body.Close() }) }
}

// output collects decoded output for session id until want appears; returns
// the text, the last event id, and whether the first frame was a reset.
func waitOutput(t *testing.T, ch <-chan termEvent, id int, want string) (string, string, bool) {
	t.Helper()
	var text strings.Builder
	lastID, sawFirst, firstReset := "", false, false
	timeout := time.After(10 * time.Second)
	for !strings.Contains(text.String(), want) {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("stream ended before %q; got %q", want, text.String())
			}
			if ev.event != "out" {
				continue
			}
			var o termOut
			if err := json.Unmarshal([]byte(ev.data), &o); err != nil {
				t.Fatal(err)
			}
			if o.S != id {
				continue
			}
			if !sawFirst {
				sawFirst, firstReset = true, o.Reset
			}
			b, _ := base64.StdEncoding.DecodeString(o.Data)
			text.Write(b)
			lastID = ev.id
		case <-timeout:
			t.Fatalf("timed out waiting for %q; got %q", want, text.String())
		}
	}
	return text.String(), lastID, firstReset
}

func TestTerminalEndToEnd(t *testing.T) {
	h := newTermHarness(t)

	// Every mutation needs the cookie, the page's Origin and a loopback Host.
	if code, _ := h.post("/api/term/open", map[string]any{"kind": "shell"}, func(r *http.Request) { r.Header.Del("Cookie") }); code != http.StatusForbidden {
		t.Errorf("open without cookie: %d, want 403", code)
	}
	if code, _ := h.post("/api/term/open", map[string]any{"kind": "shell"}, func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") }); code != http.StatusForbidden {
		t.Errorf("open from another origin: %d, want 403", code)
	}
	if code, _ := h.post("/api/term/open", map[string]any{"kind": "shell"}, func(r *http.Request) {
		r.Host = "192.168.1.5:7777"
		r.Header.Set("Origin", "http://192.168.1.5:7777")
	}); code != http.StatusForbidden {
		t.Errorf("open via a LAN address: %d, want 403", code)
	}

	events, stop := h.stream("")
	defer stop()

	code, res := h.post("/api/term/open", map[string]any{"kind": "shell", "rows": 24, "cols": 80}, nil)
	if code != http.StatusOK {
		t.Fatalf("open: %d %v", code, res)
	}
	id := int(res["id"].(float64))

	h.post("/api/term/input", map[string]any{"id": id, "data": "echo px0-$((40+2))\n"}, nil)
	_, lastID, firstReset := waitOutput(t, events, id, "px0-42")
	if !firstReset {
		t.Error("a session new to this stream should start with a reset frame")
	}
	stop()

	// Resuming from the last id replays nothing already seen.
	resumed, stopResumed := h.stream(lastID)
	h.post("/api/term/input", map[string]any{"id": id, "data": "echo second-$((1+1))\n"}, nil)
	text, _, reset := waitOutput(t, resumed, id, "second-2")
	stopResumed()
	if reset || strings.Contains(text, "px0-42") {
		t.Errorf("resume replayed old output (reset=%v): %q", reset, text)
	}

	// A fresh stream (a reloaded page) gets the scrollback, starting with a reset.
	fresh, stopFresh := h.stream("")
	text, _, reset = waitOutput(t, fresh, id, "second-2")
	if !reset || !strings.Contains(text, "px0-42") {
		t.Errorf("fresh stream should replay scrollback from a reset (reset=%v): %q", reset, text)
	}

	// Resize reaches the program.
	if code, _ := h.post("/api/term/resize", map[string]any{"id": id, "rows": 33, "cols": 101}, nil); code != http.StatusOK {
		t.Fatalf("resize: %d", code)
	}
	h.post("/api/term/input", map[string]any{"id": id, "data": "stty size\n"}, nil)
	waitOutput(t, fresh, id, "33 101")

	// At most termMaxSessions.
	for i := 1; i < termMaxSessions; i++ {
		if code, res := h.post("/api/term/open", map[string]any{"kind": "shell"}, nil); code != http.StatusOK {
			t.Fatalf("open %d: %d %v", i, code, res)
		}
	}
	if code, _ := h.post("/api/term/open", map[string]any{"kind": "shell"}, nil); code != http.StatusConflict {
		t.Errorf("open beyond the cap: %d, want 409", code)
	}

	// Closing hangs up the shell; its process is reaped.
	h.m.mu.Lock()
	s := h.m.sessions[id]
	h.m.mu.Unlock()
	if code, _ := h.post("/api/term/close", map[string]any{"id": id}, nil); code != http.StatusOK {
		t.Fatalf("close: %d", code)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		h.m.mu.Lock()
		exited := s.Exited
		h.m.mu.Unlock()
		if exited {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("closed session's shell did not exit")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if code, _ := h.post("/api/term/input", map[string]any{"id": id, "data": "x"}, nil); code != http.StatusGone {
		t.Errorf("input to a closed session: %d, want 410", code)
	}

	// Close ends the stream handler and every session.
	h.m.Close()
	for range fresh {
	}
	stopFresh()
}

func TestTerminalUnavailableReportsReason(t *testing.T) {
	m := newTermManager(t.TempDir(), "0", "/", "px0 is listening beyond this machine")
	if m.Available() || m.Token() != "" {
		t.Fatal("a terminal with a reason must be unavailable and have no token")
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/term/open", strings.NewReader(`{"kind":"shell"}`))
	m.handleOpen(rec, req)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "beyond this machine") {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}
