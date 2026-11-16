package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jiku-app/jikunode/contract"
	"github.com/jiku-app/jikunode/nodeagent"
	"github.com/jiku-app/jikunode/tunnel"
)

// Status-page constants. The listener is always 127.0.0.1; the port is the
// only configurable part (tests use an ephemeral one).
const (
	// DefaultStatusPort is the documented loopback status port.
	DefaultStatusPort = 16888
	// loopbackAddr is the only address the status server ever binds.
	loopbackAddr = "127.0.0.1"
	// DefaultLogLines is the in-memory log ring capacity.
	DefaultLogLines = 200
	// maxLogQuery caps ?n= on GET /logs.
	maxLogQuery = 2000
	// controlHeader is the CSRF header accepted on POSTs in place of a JSON
	// content type.
	controlHeader = "X-Jikunode"
	// handshakeReadyTimeout bounds how long a control action waits for the
	// tunnel handshake before giving up (the tunnel's own timeouts are 10 s).
	handshakeReadyTimeout = 15 * time.Second
)

// errNotConnected is returned by control actions while no tunnel session is
// up; the hub cannot learn a state the node never sent.
var errNotConnected = errors.New("node is not connected to the hub")

// LogRing is a fixed-capacity in-memory ring of log lines. It is an
// io.Writer so a slog handler can tee into it.
type LogRing struct {
	mu       sync.Mutex
	capacity int
	lines    []string
	pending  string
}

// NewLogRing returns a ring holding at most capacity lines.
func NewLogRing(capacity int) *LogRing {
	if capacity <= 0 {
		capacity = DefaultLogLines
	}
	return &LogRing{capacity: capacity}
}

// Write splits p into lines and keeps the newest ones.
func (r *LogRing) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	data := r.pending + string(p)
	for {
		i := strings.IndexByte(data, '\n')
		if i < 0 {
			r.pending = data
			break
		}
		r.pushLocked(data[:i])
		data = data[i+1:]
	}
	if len(r.pending) > 64<<10 {
		// A writer that never emits a newline must not grow the ring.
		r.pending = r.pending[len(r.pending)-(64<<10):]
	}
	return len(p), nil
}

func (r *LogRing) pushLocked(line string) {
	line = strings.TrimRight(line, "\r")
	r.lines = append(r.lines, line)
	if len(r.lines) > r.capacity {
		// Copy down instead of reslicing so the backing array stays bounded.
		copy(r.lines, r.lines[len(r.lines)-r.capacity:])
		r.lines = r.lines[:r.capacity]
	}
}

// Last returns up to n of the newest lines.
func (r *LogRing) Last(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n <= 0 || n > len(r.lines) {
		n = len(r.lines)
	}
	out := make([]string, n)
	copy(out, r.lines[len(r.lines)-n:])
	return out
}

// Len returns the number of buffered lines.
func (r *LogRing) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.lines)
}

// statusSnapshot is the JSON served by GET /status. It never contains the
// private key, its path, or any credential.
type statusSnapshot struct {
	NodeID        string     `json:"node_id"`
	Version       string     `json:"version"`
	HubURL        string     `json:"hub_url"`
	Bound         bool       `json:"bound"`
	Connected     bool       `json:"connected"`
	Paused        bool       `json:"paused"`
	ConnectedAt   *time.Time `json:"connected_at,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
	UptimeSeconds int64      `json:"uptime_seconds"`
}

// StatusOptions configures the loopback status server.
type StatusOptions struct {
	NodeID  string
	Version string
	HubURL  string
	Port    int
	Logs    *LogRing
}

// StatusServer serves the read-only status page, the log tail and the two
// loopback control endpoints that request pause/run from the hub.
type StatusServer struct {
	nodeID    string
	version   string
	hubURL    string
	port      int
	logs      *LogRing
	startedAt time.Time
	// paused is the hub's authoritative state, never a local guess: it is
	// written only by OnDesiredState when the hub pushes the state back.
	paused  atomic.Bool
	pushes  atomic.Uint64 // increments on every hub state push
	stateMu sync.Mutex
	stateCh chan struct{} // closed and replaced on every hub state push

	mu          sync.Mutex
	bound       bool
	connected   bool
	connectedAt time.Time
	lastError   string
	session     *tunnel.ClientSession
	ready       <-chan struct{}
}

// NewStatusServer returns a status server for one node.
func NewStatusServer(opts StatusOptions) *StatusServer {
	port := opts.Port
	if port == 0 {
		port = DefaultStatusPort
	}
	logs := opts.Logs
	if logs == nil {
		logs = NewLogRing(DefaultLogLines)
	}
	return &StatusServer{
		nodeID:    opts.NodeID,
		version:   opts.Version,
		hubURL:    opts.HubURL,
		port:      port,
		logs:      logs,
		startedAt: time.Now(),
		stateCh:   make(chan struct{}),
	}
}

// NodeID returns the bound node id.
func (s *StatusServer) NodeID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nodeID
}

// Port returns the configured status port.
func (s *StatusServer) Port() int { return s.port }

// Logs returns the log ring (shared with the process logger).
func (s *StatusServer) Logs() *LogRing { return s.logs }

// Paused reports the hub's authoritative state: it is true only after the hub
// pushed a paused state for this session.
func (s *StatusServer) Paused() bool { return s.paused.Load() }

// OnUnbound records that the runner is idling until a binding code is entered.
func (s *StatusServer) OnUnbound() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bound = false
}

// OnBound records the bound identity the runner is about to connect with.
func (s *StatusServer) OnBound(cfg *nodeagent.Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bound = true
	s.nodeID = cfg.NodeID
	s.hubURL = cfg.HubURL
}

// OnConnect records a live tunnel session and makes it the target of the
// control endpoints. ready is closed by the runner once the tunnel handshake
// completed: ClientSession.SendSetState is only safe after that point.
func (s *StatusServer) OnConnect(sess *tunnel.ClientSession, ready <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.session = sess
	s.ready = ready
	s.connected = true
	s.connectedAt = time.Now()
	s.lastError = ""
}

// OnDisconnect clears the live session. err is shown on the status page (it
// carries only tunnel/node identifiers, never secrets).
func (s *StatusServer) OnDisconnect(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.session = nil
	s.ready = nil
	s.connected = false
	if err != nil && !errors.Is(err, context.Canceled) {
		s.lastError = err.Error()
	}
}

// OnBackoff leaves the status page's disconnected state unchanged.
func (s *StatusServer) OnBackoff(error) {}

// Status returns a consistent snapshot.
func (s *StatusServer) Status() statusSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	connected := false
	if s.connected && s.ready != nil {
		select {
		case <-s.ready:
			connected = true
		default:
		}
	}
	snap := statusSnapshot{
		NodeID:        s.nodeID,
		Version:       s.version,
		HubURL:        s.hubURL,
		Bound:         s.bound,
		Connected:     connected,
		Paused:        s.paused.Load(),
		LastError:     s.lastError,
		UptimeSeconds: int64(time.Since(s.startedAt).Seconds()),
	}
	if connected && !s.connectedAt.IsZero() {
		at := s.connectedAt
		snap.ConnectedAt = &at
	}
	return snap
}

// SetState asks the hub to change this node's run state. The hub is
// authoritative: the local gate changes only when the hub pushes the state
// back, so SetState waits for that push instead of recording it locally.
func (s *StatusServer) SetState(ctx context.Context, st contract.RunState) error {
	s.mu.Lock()
	sess, ready := s.session, s.ready
	s.mu.Unlock()
	if sess == nil {
		return errNotConnected
	}
	if ready != nil {
		select {
		case <-ready:
		case <-time.After(handshakeReadyTimeout):
			return errors.New("tunnel handshake did not complete")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	// The wait must be satisfied by a push that arrives after this request:
	// an already-matching local state is not proof that the hub applied it.
	generation := s.pushes.Load()
	if err := sess.SendSetState(st); err != nil {
		return fmt.Errorf("send state to hub: %w", err)
	}
	if err := s.waitState(ctx, st == contract.StatePaused, generation); err != nil {
		return fmt.Errorf("hub did not apply %s: %w", st, err)
	}
	return nil
}

// OnDesiredState applies the hub's authoritative run state and wakes every
// control action waiting for it.
func (s *StatusServer) OnDesiredState(st contract.RunState) {
	s.paused.Store(st == contract.StatePaused)
	s.pushes.Add(1)
	s.stateMu.Lock()
	close(s.stateCh)
	s.stateCh = make(chan struct{})
	s.stateMu.Unlock()
}

// waitState blocks until a hub push newer than generation leaves the state at
// want (or ctx ends).
func (s *StatusServer) waitState(ctx context.Context, want bool, generation uint64) error {
	for {
		s.stateMu.Lock()
		ch := s.stateCh
		s.stateMu.Unlock()
		if s.paused.Load() == want && s.pushes.Load() > generation {
			return nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// statusRoute is one registered route. The list is the complete surface: no
// route accepts or displays a credential, token or key.
type statusRoute struct {
	method  string
	pattern string
	handle  func(*StatusServer, http.ResponseWriter, *http.Request)
}

// statusRoutes is the exhaustive route table (enumerated by
// TestNoCredentialEndpoints).
var statusRoutes = []statusRoute{
	{http.MethodGet, "/", (*StatusServer).handleIndex},
	{http.MethodGet, "/status", (*StatusServer).handleStatus},
	{http.MethodGet, "/logs", (*StatusServer).handleLogs},
	{http.MethodPost, "/control/pause", (*StatusServer).handlePause},
	{http.MethodPost, "/control/resume", (*StatusServer).handleResume},
}

// Handler returns the guarded status handler.
func (s *StatusServer) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, rt := range statusRoutes {
		rt := rt
		mux.HandleFunc(rt.method+" "+rt.pattern, func(w http.ResponseWriter, r *http.Request) {
			rt.handle(s, w, r)
		})
	}
	return s.guard(mux)
}

// guard rejects requests whose Host header is not loopback (DNS-rebinding
// defence) and POSTs that carry neither a JSON content type nor the
// X-Jikunode header (CSRF defence).
func (s *StatusServer) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if !isLoopbackHost(r.Host) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodPost && !csrfOK(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isLoopbackHost reports whether a Host header names the loopback interface.
func isLoopbackHost(hostport string) bool {
	if hostport == "" {
		return false
	}
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// csrfOK requires a JSON content type or the explicit control header.
func csrfOK(r *http.Request) bool {
	if strings.TrimSpace(r.Header.Get(controlHeader)) == "1" {
		return true
	}
	ct := r.Header.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.EqualFold(strings.TrimSpace(ct), "application/json")
}

// Listen binds the status listener on 127.0.0.1:<port>. Port 0 means an
// ephemeral port (tests).
func (s *StatusServer) Listen() (net.Listener, error) {
	return s.ListenAt(net.JoinHostPort(loopbackAddr, strconv.Itoa(s.port)))
}

// ListenAt binds addr, refusing anything that is not the loopback IP. This is
// the only place a listener is created, so a non-loopback bind is impossible
// even through misconfiguration.
func (s *StatusServer) ListenAt(addr string) (net.Listener, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("status address %q: %w", addr, err)
	}
	if host != loopbackAddr {
		return nil, fmt.Errorf("status server must bind %s, refusing %q", loopbackAddr, addr)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 0 || port > 65535 {
		return nil, fmt.Errorf("status port %q out of range", portStr)
	}
	return net.Listen("tcp", net.JoinHostPort(loopbackAddr, strconv.Itoa(port)))
}

// Serve serves the status handler on ln until ctx is done.
func (s *StatusServer) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return srv.Shutdown(shutCtx)
	}
}

func (s *StatusServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		// "GET /" is a subtree pattern; every other path is not a route.
		http.NotFound(w, r)
		return
	}
	snap := s.Status()
	state := "running"
	if snap.Paused {
		state = "paused"
	}
	binding := "unbound (run: jikunode bind XXXXX-XXXXX)"
	if snap.Bound {
		binding = "bound"
	}
	conn := "disconnected"
	if snap.Connected {
		conn = "connected"
	}
	var b strings.Builder
	b.WriteString("<!doctype html><html><head><meta charset=\"utf-8\">")
	b.WriteString("<title>jikunode</title></head><body>")
	b.WriteString("<h1>jikunode</h1><table>")
	fmt.Fprintf(&b, "<tr><th>node</th><td>%s</td></tr>", html.EscapeString(snap.NodeID))
	fmt.Fprintf(&b, "<tr><th>version</th><td>%s</td></tr>", html.EscapeString(snap.Version))
	fmt.Fprintf(&b, "<tr><th>hub</th><td>%s</td></tr>", html.EscapeString(snap.HubURL))
	fmt.Fprintf(&b, "<tr><th>binding</th><td>%s</td></tr>", html.EscapeString(binding))
	fmt.Fprintf(&b, "<tr><th>connection</th><td>%s</td></tr>", conn)
	fmt.Fprintf(&b, "<tr><th>state</th><td>%s</td></tr>", state)
	b.WriteString("</table><p>")
	b.WriteString("<button onclick=\"control('pause')\">Pause</button> ")
	b.WriteString("<button onclick=\"control('resume')\">Resume</button></p>")
	b.WriteString("<p><a href=\"/status\">status JSON</a> &middot; <a href=\"/logs\">logs</a></p>")
	b.WriteString("<script>function control(a){fetch('/control/'+a,{method:'POST',headers:{'X-Jikunode':'1'}}).then(function(){location.reload()})}</script>")
	b.WriteString("</body></html>")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, b.String())
}

func (s *StatusServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Status())
}

func (s *StatusServer) handleLogs(w http.ResponseWriter, r *http.Request) {
	n := 0
	if raw := r.URL.Query().Get("n"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
			return
		}
		n = v
		if n > maxLogQuery {
			n = maxLogQuery
		}
	}
	writeJSON(w, http.StatusOK, map[string][]string{"lines": s.logs.Last(n)})
}

func (s *StatusServer) handlePause(w http.ResponseWriter, r *http.Request) {
	s.control(w, r, contract.StatePaused)
}

func (s *StatusServer) handleResume(w http.ResponseWriter, r *http.Request) {
	s.control(w, r, contract.StateRunning)
}

func (s *StatusServer) control(w http.ResponseWriter, r *http.Request, st contract.RunState) {
	if err := s.SetState(r.Context(), st); err != nil {
		if errors.Is(err, errNotConnected) {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "not_connected"})
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "send_failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"state": string(st)})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ---- CLI client helpers (all loopback) --------------------------------------

// loopbackBaseURL is the only address the CLI talks to for control.
func loopbackBaseURL(port int) string {
	return "http://" + net.JoinHostPort(loopbackAddr, strconv.Itoa(port))
}

func statusClient() *http.Client { return &http.Client{Timeout: 5 * time.Second} }

// controlNode sends POST /control/<action> with the CSRF header.
func controlNode(ctx context.Context, port int, action string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		loopbackBaseURL(port)+"/control/"+action, strings.NewReader("{}"))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(controlHeader, "1")
	resp, err := statusClient().Do(req)
	if err != nil {
		return fmt.Errorf("contact status server on %s: %w", loopbackBaseURL(port), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s failed: %s", action, statusError(resp))
	}
	return nil
}

// fetchStatus reads GET /status.
func fetchStatus(ctx context.Context, port int) (statusSnapshot, error) {
	var snap statusSnapshot
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, loopbackBaseURL(port)+"/status", nil)
	if err != nil {
		return snap, err
	}
	resp, err := statusClient().Do(req)
	if err != nil {
		return snap, fmt.Errorf("contact status server on %s: %w", loopbackBaseURL(port), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return snap, fmt.Errorf("status failed: %s", statusError(resp))
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&snap); err != nil {
		return snap, fmt.Errorf("decode status: %w", err)
	}
	return snap, nil
}

// fetchLogs reads GET /logs?n=.
func fetchLogs(ctx context.Context, port, n int) ([]string, error) {
	url := loopbackBaseURL(port) + "/logs"
	if n > 0 {
		url += "?n=" + strconv.Itoa(n)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := statusClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("contact status server on %s: %w", loopbackBaseURL(port), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("logs failed: %s", statusError(resp))
	}
	var body struct {
		Lines []string `json:"lines"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode logs: %w", err)
	}
	return body.Lines, nil
}

// statusError renders a non-200 status response using its error code only.
func statusError(resp *http.Response) string {
	var body struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&body); err == nil && body.Error != "" {
		return body.Error
	}
	return resp.Status
}
