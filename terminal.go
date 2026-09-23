package main

// Terminal Sessions: interactive pseudo-terminals hosted by the px0 process and
// shown in the browser's terminal pane, running the user's shell (a Shell
// Session) or the selected harness (a Harness Session) in the workspace. px0
// relays keystrokes the user types and never submits input on its own; the one
// thing it types unprompted is a pasted Reference, which the user must still
// send. See docs/internals/terminal.md.
//
// Transport is one Server-Sent Events stream for every session plus small JSON
// POSTs for input, so it needs no WebSocket and works behind any proxy that
// carries /api/stream. Each session keeps its recent output in a ring buffer
// addressed by a running byte offset; a client resumes from the offsets it has
// seen, and one that falls behind the ring is reset and replayed rather than
// ever blocking the PTY reader.

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	termMaxSessions = 4
	termRingBytes   = 256 << 10 // scrollback replayed to a reconnecting browser, per session
	termFrameBytes  = 48 << 10  // output per SSE frame, before base64
	termInputBytes  = 64 << 10  // largest accepted input POST
	termHangupGrace = 2 * time.Second
)

// termInfo is what the browser knows about a session.
type termInfo struct {
	ID     int    `json:"id"`
	Kind   string `json:"kind"` // "shell" or "harness"
	Title  string `json:"title"`
	Exited bool   `json:"exited"`
	Code   int    `json:"code"`
}

// termSession is one pseudo-terminal and the process leading it. Exited, Code,
// ring, end and closedBy are guarded by termManager.mu.
type termSession struct {
	termInfo
	cmd      *exec.Cmd
	pty      *os.File
	ptyOnce  sync.Once
	ring     []byte // circular: output byte n lives at ring[n % len(ring)]
	end      int64  // total bytes of output ever produced
	closedBy bool   // closed from px0 rather than by its own exit
}

// oldest is the offset of the first byte the ring still holds.
func (s *termSession) oldest() int64 {
	if n := int64(len(s.ring)); s.end > n {
		return s.end - n
	}
	return 0
}

func (s *termSession) write(p []byte) {
	n := len(s.ring)
	if len(p) > n {
		s.end += int64(len(p) - n)
		p = p[len(p)-n:]
	}
	at := int(s.end % int64(n))
	c := copy(s.ring[at:], p)
	copy(s.ring, p[c:])
	s.end += int64(len(p))
}

// read copies output from offset from (at least oldest) up to end, at most max bytes.
func (s *termSession) read(from int64, max int) []byte {
	if from < s.oldest() {
		from = s.oldest()
	}
	size := s.end - from
	if size > int64(max) {
		size = int64(max)
	}
	out := make([]byte, size)
	n := int64(len(s.ring))
	at := int(from % n)
	c := copy(out, s.ring[at:])
	copy(out[c:], s.ring)
	return out
}

func (s *termSession) closePTY() { s.ptyOnce.Do(func() { s.pty.Close() }) }

// termManager owns every Terminal Session of this px0 process. They live as long
// as the process does (or until closed in the UI) and die with it: nothing
// outlives px0 and nothing is written to disk.
type termManager struct {
	root   string
	token  string // per-process secret; the browser holds it as an HttpOnly cookie
	cookie string // cookie name, unique per port since cookies ignore ports
	path   string // cookie path: the base path px0 is served under
	// off explains why the terminal is unavailable; empty when it is available.
	off string
	// harness resolves the selected harness's interactive command, or fails.
	harness func() (title string, argv, env []string, err error)
	// onQuiet pokes the git watcher once a session's output pauses, which is
	// usually a harness finishing a step, so its edits reach open tabs without
	// waiting for the next adaptive poll. Not on every chunk: a spinner alone
	// would run git status several times a second.
	onQuiet func()
	logf    func(role, msg, detail string)

	mu       sync.Mutex
	sessions map[int]*termSession
	starting int // sessions being started, counted against termMaxSessions
	nextID   int
	version  int64         // bumps whenever the session list changes
	wake     chan struct{} // closed and replaced on any output or list change
	closed   bool
}

// newTermManager prepares the terminal for this process. off, when non-empty,
// leaves it visible but unavailable with that reason (flag, setting, platform,
// or a non-loopback listener).
func newTermManager(root, port, basePath, off string) *termManager {
	if off == "" && !ptySupported {
		off = "the terminal is not supported on this platform yet"
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		off = "no randomness for an access token: " + err.Error()
	}
	return &termManager{
		root:     root,
		token:    hex.EncodeToString(b),
		cookie:   "px0_term_" + port,
		path:     cleanBasePath(basePath),
		off:      off,
		sessions: map[int]*termSession{},
		wake:     make(chan struct{}),
	}
}

// Available reports whether sessions can be opened at all in this process.
func (m *termManager) Available() bool { return m != nil && m.off == "" }

// Token is added to the URL px0 opens and prints, so that page, and only it,
// can reach the terminal. Empty when the terminal is unavailable.
func (m *termManager) Token() string {
	if !m.Available() {
		return ""
	}
	return m.token
}

// signal wakes every stream. Callers hold m.mu.
func (m *termManager) signal() {
	close(m.wake)
	m.wake = make(chan struct{})
}

func (m *termManager) open(kind string, rows, cols int) (*termSession, error) {
	var title string
	var argv, env []string
	switch kind {
	case "shell":
		sh := os.Getenv("SHELL")
		if sh == "" {
			sh = "/bin/sh"
		}
		title, argv = filepath.Base(sh), []string{sh, "-l"}
	case "harness":
		if m.harness == nil {
			return nil, errors.New("no coding harness is configured")
		}
		var err error
		if title, argv, env, err = m.harness(); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unknown session kind %q", kind)
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errors.New("px0 is shutting down")
	}
	if len(m.sessions)+m.starting >= termMaxSessions {
		m.mu.Unlock()
		return nil, fmt.Errorf("at most %d terminal sessions; close one first", termMaxSessions)
	}
	m.nextID++
	m.starting++
	id := m.nextID
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.starting--
		m.mu.Unlock()
	}()

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = m.root
	cmd.Env = append(os.Environ(), "TERM=xterm-256color", "COLORTERM=truecolor", "TERM_PROGRAM=px0", "TERM_PROGRAM_VERSION="+version)
	cmd.Env = append(cmd.Env, env...)
	master, err := startInPTY(cmd, rows, cols)
	if err != nil {
		return nil, fmt.Errorf("start %s: %w", title, err)
	}
	s := &termSession{termInfo: termInfo{ID: id, Kind: kind, Title: title}, cmd: cmd, pty: master, ring: make([]byte, termRingBytes)}

	m.mu.Lock()
	if m.closed { // shut down while starting
		m.mu.Unlock()
		s.closePTY()
		signalGroup(cmd.Process.Pid, syscall.SIGKILL)
		go cmd.Wait()
		return nil, errors.New("px0 is shutting down")
	}
	m.sessions[id] = s
	m.version++
	m.signal()
	m.mu.Unlock()

	go m.pump(s)
	go m.reap(s)
	m.log("info", "terminal", fmt.Sprintf("started %s (session %d)", title, id))
	return s, nil
}

// pump copies the child's output into the ring until the terminal closes.
func (m *termManager) pump(s *termSession) {
	var quiet *time.Timer
	if m.onQuiet != nil {
		quiet = time.AfterFunc(time.Hour, m.onQuiet)
		quiet.Stop()
		defer quiet.Stop()
	}
	buf := make([]byte, 32<<10)
	for {
		n, err := s.pty.Read(buf)
		if n > 0 {
			m.mu.Lock()
			s.write(buf[:n])
			m.signal()
			m.mu.Unlock()
			if quiet != nil {
				quiet.Reset(400 * time.Millisecond)
			}
		}
		if err != nil {
			// EIO once every holder of the terminal has exited, or the master
			// was closed. The ring stays for the pane to show.
			s.closePTY()
			return
		}
	}
}

// reap waits for the session's leader, so it never lingers as a zombie, and
// records how it ended.
func (m *termManager) reap(s *termSession) {
	err := s.cmd.Wait()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	}
	m.mu.Lock()
	s.Exited, s.Code = true, code
	by := s.closedBy
	m.version++
	m.signal()
	m.mu.Unlock()
	if !by {
		m.log("info", "terminal", fmt.Sprintf("%s exited with code %d (session %d)", s.Title, code, s.ID))
	}
	if m.onQuiet != nil {
		m.onQuiet()
	}
}

// terminate hangs up the session's terminal, which signals its foreground
// processes the way closing a terminal window does, and kills whatever is left
// of its process group after a grace period. A leader already reaped is not
// signalled: its pid may belong to someone else by now.
func (m *termManager) terminate(s *termSession) {
	s.closePTY()
	m.mu.Lock()
	exited := s.Exited
	m.mu.Unlock()
	if exited {
		return
	}
	pid := s.cmd.Process.Pid
	signalGroup(pid, syscall.SIGHUP)
	time.AfterFunc(termHangupGrace, func() {
		m.mu.Lock()
		exited := s.Exited
		m.mu.Unlock()
		// ponytail: reap() sets Exited just after Wait frees the pid, so a group
		// kill in that instant could reach a reused pgid; a pidfd would close it.
		if !exited {
			signalGroup(pid, syscall.SIGKILL)
		}
	})
}

func (m *termManager) closeSession(id int) error {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if ok {
		delete(m.sessions, id)
		s.closedBy = true
		m.version++
		m.signal()
	}
	m.mu.Unlock()
	if !ok {
		return errors.New("no such session")
	}
	m.terminate(s)
	return nil
}

// Close ends every session; main calls it on the way out, before a PR
// checkout the sessions may be running in is removed.
func (m *termManager) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	all := make([]*termSession, 0, len(m.sessions))
	for _, s := range m.sessions {
		s.closedBy = true
		all = append(all, s)
	}
	m.signal() // releases every stream
	m.mu.Unlock()

	exited := func(s *termSession) bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return s.Exited
	}
	for _, s := range all {
		s.closePTY()
		if !exited(s) {
			signalGroup(s.cmd.Process.Pid, syscall.SIGHUP)
		}
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for _, s := range all {
		for !exited(s) && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if !exited(s) {
			signalGroup(s.cmd.Process.Pid, syscall.SIGKILL)
		}
	}
}

// session returns a session whose process is still running.
func (m *termManager) session(id int) (*termSession, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	return s, ok && !s.Exited
}

func (m *termManager) log(role, label, msg string) {
	if m.logf != nil {
		m.logf(role, label, msg)
	}
}

// ---------------------------------------------------------------- access

// isLoopbackHost accepts only names that can reach this machine and nothing
// else: localhost or a loopback IP. Any other IP, even this machine's own LAN
// address, means the page is being used from elsewhere.
func isLoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Adopt handles the token px0 put in the URL it opened: a matching token is
// exchanged for the cookie, and the token is always stripped from the address
// bar with a redirect. Returns true when it wrote the response.
func (m *termManager) Adopt(w http.ResponseWriter, r *http.Request) bool {
	q := r.URL.Query()
	t := q.Get("t")
	if t == "" {
		return false
	}
	if m.Available() && isLoopbackHost(r.Host) && subtle.ConstantTimeCompare([]byte(t), []byte(m.token)) == 1 {
		http.SetCookie(w, &http.Cookie{
			Name:     m.cookie,
			Value:    m.token,
			Path:     m.path,
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
		})
	}
	q.Del("t")
	u := url.URL{Path: r.URL.Path, RawQuery: q.Encode()}
	http.Redirect(w, r, u.String(), http.StatusSeeOther)
	return true
}

// Authorized reports whether r comes from the page px0 opened, on this machine.
func (m *termManager) Authorized(r *http.Request) bool {
	if !m.Available() || !isLoopbackHost(r.Host) {
		return false
	}
	c, err := r.Cookie(m.cookie)
	return err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(m.token)) == 1
}

// guard admits a terminal request, and for a POST also requires the page's own
// Origin, as localPost does for other endpoints that run commands.
func (m *termManager) guard(w http.ResponseWriter, r *http.Request, post bool) bool {
	if m == nil {
		fail(w, http.StatusNotFound, "terminal is not enabled")
		return false
	}
	if !m.Available() {
		fail(w, http.StatusServiceUnavailable, m.off)
		return false
	}
	if post {
		if r.Method != http.MethodPost {
			fail(w, http.StatusMethodNotAllowed, "POST only")
			return false
		}
		if o, err := url.Parse(r.Header.Get("Origin")); err != nil || o.Host != r.Host {
			fail(w, http.StatusForbidden, "request did not come from px0")
			return false
		}
	}
	if !m.Authorized(r) {
		fail(w, http.StatusForbidden, "open px0 on this machine from the URL it printed (it carries the terminal's access token)")
		return false
	}
	return true
}

// Meta is the pane's view of the terminal for /api/meta.
func (m *termManager) Meta(r *http.Request) map[string]any {
	if m == nil {
		return map[string]any{"available": false, "reason": "disabled with -no-terminal or the terminal.enabled setting"}
	}
	authorized := m.Authorized(r)
	out := map[string]any{"available": m.Available(), "authorized": authorized}
	if m.off != "" {
		out["reason"] = m.off
	}
	if authorized {
		// Lets a reloaded page restore its pane without opening the stream first.
		m.mu.Lock()
		list := make([]termInfo, 0, len(m.sessions))
		for _, s := range m.sessions {
			list = append(list, s.termInfo)
		}
		m.mu.Unlock()
		sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
		out["sessions"] = list
	}
	return out
}

// ---------------------------------------------------------------- HTTP

type termRequest struct {
	ID   int    `json:"id"`
	Kind string `json:"kind"`
	Rows int    `json:"rows"`
	Cols int    `json:"cols"`
	Data string `json:"data"`
	// Bin marks Data as bytes carried one per character (xterm.js onBinary,
	// used for some mouse reports) rather than UTF-8 text.
	Bin bool `json:"bin"`
}

func decodeTermRequest(w http.ResponseWriter, r *http.Request) (termRequest, bool) {
	var req termRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, termInputBytes+1024)).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "bad request: "+err.Error())
		return req, false
	}
	return req, true
}

func (m *termManager) handleOpen(w http.ResponseWriter, r *http.Request) {
	if !m.guard(w, r, true) {
		return
	}
	req, ok := decodeTermRequest(w, r)
	if !ok {
		return
	}
	s, err := m.open(req.Kind, req.Rows, req.Cols)
	if err != nil {
		fail(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, map[string]any{"id": s.ID, "title": s.Title, "kind": s.Kind})
}

func (m *termManager) handleInput(w http.ResponseWriter, r *http.Request) {
	if !m.guard(w, r, true) {
		return
	}
	req, ok := decodeTermRequest(w, r)
	if !ok {
		return
	}
	s, live := m.session(req.ID)
	if !live {
		fail(w, http.StatusGone, "session has ended")
		return
	}
	data := []byte(req.Data)
	if req.Bin {
		data = make([]byte, 0, len(req.Data))
		for _, c := range req.Data {
			data = append(data, byte(c))
		}
	}
	// Written outside the lock: a program not reading its input can fill the
	// terminal's buffer, and that must stall only this request.
	if _, err := s.pty.Write(data); err != nil {
		fail(w, http.StatusGone, "session has ended")
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (m *termManager) handleResize(w http.ResponseWriter, r *http.Request) {
	if !m.guard(w, r, true) {
		return
	}
	req, ok := decodeTermRequest(w, r)
	if !ok {
		return
	}
	s, live := m.session(req.ID)
	if !live {
		fail(w, http.StatusGone, "session has ended")
		return
	}
	if err := setWinsize(s.pty, req.Rows, req.Cols); err != nil {
		fail(w, http.StatusGone, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (m *termManager) handleClose(w http.ResponseWriter, r *http.Request) {
	if !m.guard(w, r, true) {
		return
	}
	req, ok := decodeTermRequest(w, r)
	if !ok {
		return
	}
	if err := m.closeSession(req.ID); err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// parseCursors reads "id:offset,id:offset", the SSE id px0 stamps on every
// output frame. The browser sends it back as Last-Event-ID on reconnect, or as
// ?since= when it opens a fresh stream itself.
func parseCursors(s string) map[int]int64 {
	out := map[int]int64{}
	for _, part := range strings.Split(s, ",") {
		id, off, ok := strings.Cut(part, ":")
		if !ok {
			continue
		}
		i, err1 := strconv.Atoi(id)
		o, err2 := strconv.ParseInt(off, 10, 64)
		if err1 == nil && err2 == nil && o >= 0 {
			out[i] = o
		}
	}
	return out
}

func formatCursors(c map[int]int64) string {
	ids := make([]int, 0, len(c))
	for id := range c {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	var b strings.Builder
	for i, id := range ids {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Itoa(id))
		b.WriteByte(':')
		b.WriteString(strconv.FormatInt(c[id], 10))
	}
	return b.String()
}

type termOut struct {
	S     int    `json:"s"`
	Reset bool   `json:"r,omitempty"` // clear the screen before writing: bytes before these are gone
	Data  string `json:"d"`
}

// handleStream multiplexes every session's output onto one SSE stream: one
// connection per session would soon exhaust the browser's six per origin.
func (m *termManager) handleStream(w http.ResponseWriter, r *http.Request) {
	if !m.guard(w, r, false) {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		fail(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	since := r.Header.Get("Last-Event-ID")
	if since == "" {
		since = r.URL.Query().Get("since")
	}
	cursors := parseCursors(since)
	seenVersion := int64(-1)
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return
		}
		wake := m.wake
		var list []termInfo
		if m.version != seenVersion {
			seenVersion = m.version
			list = make([]termInfo, 0, len(m.sessions))
			for _, s := range m.sessions {
				list = append(list, s.termInfo)
			}
			sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
			for id := range cursors {
				if _, ok := m.sessions[id]; !ok {
					delete(cursors, id)
				}
			}
		}
		// Each frame's id holds every cursor as of that frame, so a browser that
		// drops mid-batch resumes exactly after the last frame it received.
		type frame struct {
			id  string
			out termOut
		}
		var frames []frame
		ids := make([]int, 0, len(m.sessions))
		for id := range m.sessions {
			ids = append(ids, id)
		}
		sort.Ints(ids)
		for _, id := range ids {
			s := m.sessions[id]
			from, known := cursors[id]
			// Unknown to this browser, or fallen out of the ring: start over from
			// the oldest byte held, and tell the pane to clear what it shows.
			reset := !known || from < s.oldest() || from > s.end
			if reset {
				from = s.oldest()
			}
			for first := true; from < s.end || (reset && first); first = false {
				chunk := s.read(from, termFrameBytes)
				from += int64(len(chunk))
				cursors[id] = from
				frames = append(frames, frame{formatCursors(cursors), termOut{S: id, Reset: reset && first, Data: base64.StdEncoding.EncodeToString(chunk)}})
			}
		}
		m.mu.Unlock()

		if list != nil {
			b, _ := json.Marshal(map[string]any{"sessions": list})
			if _, err := fmt.Fprintf(w, "event: sessions\ndata: %s\n\n", b); err != nil {
				return
			}
		}
		for _, f := range frames {
			b, _ := json.Marshal(f.out)
			if _, err := fmt.Fprintf(w, "id: %s\nevent: out\ndata: %s\n\n", f.id, b); err != nil {
				return
			}
		}
		flusher.Flush()

		select {
		case <-wake:
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
