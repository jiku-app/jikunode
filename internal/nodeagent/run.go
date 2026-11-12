package nodeagent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/jiku-app/jikunode/internal/tunnel"
)

// Connection and backoff constants. The backoff doubles from MinBackoff to
// MaxBackoff, is jittered, and resets only after a session stayed up for
// StableAfter so a flapping hub cannot drive a reconnect storm.
const (
	// DefaultMinBackoff is the first reconnect delay.
	DefaultMinBackoff = time.Second
	// DefaultMaxBackoff caps the reconnect delay.
	DefaultMaxBackoff = 60 * time.Second
	// DefaultStableAfter is how long a session must last to reset the
	// backoff.
	DefaultStableAfter = 60 * time.Second
	// DefaultDialTimeout bounds one websocket dial.
	DefaultDialTimeout = 15 * time.Second
	// DefaultPollInterval is how often an unbound runner checks for the
	// bound marker. The check is a local stat, never a network call.
	DefaultPollInterval = 2 * time.Second
	// tunnelPath is the hub's node-tunnel websocket route.
	tunnelPath = "/v1/node/tunnel"
	// bindPath is the hub's binding route.
	bindPath = "/v1/node/bind"
)

// errPaused is returned for every new stream while the node is paused.
var errPaused = errors.New("node is paused")

// Hooks observes tunnel lifecycle and optionally gates new streams while paused.
type Hooks interface {
	// OnUnbound is called when the runner starts idling because the node is
	// not bound. OnBound is called when the bound marker appears.
	OnUnbound()
	OnBound(*Config)
	OnConnect(*tunnel.ClientSession, <-chan struct{})
	OnDisconnect(error)
	OnBackoff(error)
	Paused() bool
}

// Runner keeps one node connected to the hub: dial, authenticate, serve
// streams, and reconnect with jittered exponential backoff until ctx ends.
// Until the node is bound (the bound marker exists in Dir) it idles and makes
// no network call at all; the hub URL, node id and key are read from Dir each
// time a session starts. The bound marker and config are read only before each
// connect: a change to them while a session is live does not affect that
// session and takes effect at the next reconnect.
type Runner struct {
	Dir               string
	Version           string
	Policy            *tunnel.DialPolicy
	Logger            *slog.Logger
	Hooks             Hooks
	Network           Network
	HeartbeatInterval time.Duration

	// Tunables; zero means the default.
	MinBackoff  time.Duration
	MaxBackoff  time.Duration
	StableAfter time.Duration
	// PollInterval is the bound-marker poll period while unbound.
	PollInterval time.Duration

	// Rand returns jitter entropy in [0,1); nil means math/rand.Float64.
	Rand func() float64
	// Dial overrides the websocket dial (tests point it at an in-process
	// hub); nil means websocket.Dial.
	Dial func(ctx context.Context, url string) (*websocket.Conn, error)
}

func (r *Runner) minBackoff() time.Duration {
	if r.MinBackoff <= 0 {
		return DefaultMinBackoff
	}
	return r.MinBackoff
}

func (r *Runner) maxBackoff() time.Duration {
	if r.MaxBackoff <= 0 {
		return DefaultMaxBackoff
	}
	if r.MaxBackoff < r.minBackoff() {
		return r.minBackoff()
	}
	return r.MaxBackoff
}

func (r *Runner) stableAfter() time.Duration {
	if r.StableAfter <= 0 {
		return DefaultStableAfter
	}
	return r.StableAfter
}

func (r *Runner) pollInterval() time.Duration {
	if r.PollInterval <= 0 {
		return DefaultPollInterval
	}
	return r.PollInterval
}

func (r *Runner) logger() *slog.Logger {
	if r.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return r.Logger
}

// Run connects until ctx is cancelled, reconnecting with jittered
// exponential backoff. It returns ctx.Err() on cancellation.
func (r *Runner) Run(ctx context.Context) error {
	attempt := 0
	for {
		if err := r.waitBound(ctx); err != nil {
			return err
		}
		started := time.Now()
		err := r.connect(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Since(started) >= r.stableAfter() {
			attempt = 0
		}
		delay := BackoffDelay(attempt, r.minBackoff(), r.maxBackoff(), r.Rand)
		if r.Hooks != nil {
			r.Hooks.OnBackoff(err)
		}
		attempt++
		r.logger().Warn("jikunode: reconnect scheduled",
			"event", "reconnect_scheduled",
			"attempt", attempt, "delay_ms", delay.Milliseconds(), "error", err.Error())
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
}

// waitBound returns once the bound marker exists. While it does not, the
// runner is idle: it polls the marker with a local stat and nothing else (no
// discovery, no DNS, no dial).
func (r *Runner) waitBound(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if IsBound(r.Dir) {
		return nil
	}
	if r.Hooks != nil {
		r.Hooks.OnUnbound()
	}
	r.logger().Info("jikunode: not bound; idling until a binding code is entered", "event", "unbound")
	t := time.NewTicker(r.pollInterval())
	defer t.Stop()
	for !IsBound(r.Dir) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
	return nil
}

// connect runs one session: load the bound identity from disk, dial,
// handshake, serve until it ends.
func (r *Runner) connect(ctx context.Context) error {
	cfg, err := LoadConfig(r.Dir)
	if err != nil {
		return err
	}
	key, err := LoadPrivateKey(cfg.KeyPath)
	if err != nil {
		return err
	}
	if r.Hooks != nil {
		r.Hooks.OnBound(cfg)
	}
	endpoint, err := TunnelURL(cfg.HubURL)
	if err != nil {
		return err
	}
	dial := r.Dial
	if dial == nil {
		dial = func(ctx context.Context, u string) (*websocket.Conn, error) {
			conn, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPClient: r.Network.Client(nil)})
			return conn, err
		}
	}
	dialCtx, cancel := context.WithTimeout(ctx, DefaultDialTimeout)
	ws, err := dial(dialCtx, endpoint)
	cancel()
	if err != nil {
		return fmt.Errorf("dial hub: %w", err)
	}
	// The handshake runs inside ClientSession.Run, so readiness for
	// SendSetState is signalled by the tunnel's own "session established"
	// event: it is logged on the Run goroutine after the control stream is
	// live, and closing ready there orders every later SendSetState after it.
	logger := r.logger()
	var ready *handshakeReady
	if r.Hooks != nil {
		ready = newHandshakeReady()
		logger = slog.New(&handshakeReadyHandler{Handler: logger.Handler(), ready: ready})
	}
	sess, err := tunnel.NewClientSession(tunnel.ClientConfig{
		NodeID:            cfg.NodeID,
		Version:           r.Version,
		PrivateKey:        key,
		Policy:            r.policy(),
		HeartbeatInterval: r.HeartbeatInterval,
		Logger:            logger,
	})
	if err != nil {
		_ = ws.Close(websocket.StatusInternalError, "")
		return err
	}
	if r.Hooks != nil {
		r.Hooks.OnConnect(sess, ready.channel())
	}
	r.logger().Info("jikunode: connected", "event", "session_established", "node_id", cfg.NodeID)
	err = sess.Run(ctx, ws)
	if r.Hooks != nil {
		r.Hooks.OnDisconnect(err)
	}
	return err
}

// handshakeReady is closed exactly once, when the tunnel handshake completed.
// ClientSession.SendSetState writes the control stream, which only exists after
// that point, so the control endpoints wait on the channel.
type handshakeReady struct {
	once sync.Once
	ch   chan struct{}
}

func newHandshakeReady() *handshakeReady {
	return &handshakeReady{ch: make(chan struct{})}
}

func (r *handshakeReady) done() { r.once.Do(func() { close(r.ch) }) }

func (r *handshakeReady) channel() <-chan struct{} { return r.ch }

// handshakeReadyHandler closes ready when the tunnel reports a completed
// handshake ("event=session_established", logged on the Run goroutine after
// the control stream is live).
type handshakeReadyHandler struct {
	slog.Handler
	ready *handshakeReady
}

// Enabled reports true so the readiness signal never depends on the
// configured log level; Handle still applies the wrapped handler's filter.
func (h *handshakeReadyHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return true
}

func (h *handshakeReadyHandler) Handle(ctx context.Context, rec slog.Record) error {
	rec.Attrs(func(a slog.Attr) bool {
		if a.Key == "event" && a.Value.String() == "session_established" {
			h.ready.done()
			return false
		}
		return true
	})
	if !h.Handler.Enabled(ctx, rec.Level) {
		return nil
	}
	return h.Handler.Handle(ctx, rec)
}

// WithAttrs and WithGroup keep the readiness hook in the handler chain.
func (h *handshakeReadyHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &handshakeReadyHandler{Handler: h.Handler.WithAttrs(attrs), ready: h.ready}
}

func (h *handshakeReadyHandler) WithGroup(name string) slog.Handler {
	return &handshakeReadyHandler{Handler: h.Handler.WithGroup(name), ready: h.ready}
}

// policy returns the per-stream destination policy, gated by the local pause
// state: while paused every new stream is refused before DNS resolution.
func (r *Runner) policy() *tunnel.DialPolicy {
	base := r.Policy
	if base == nil {
		base = tunnel.DefaultDialPolicy()
	}
	if r.Network.Resolver != nil {
		copyPolicy := *base
		copyPolicy.Resolver = r.Network.Resolver
		base = &copyPolicy
	}
	if r.Hooks == nil {
		return base
	}
	paused := r.Hooks.Paused
	gated := &tunnel.DialPolicy{
		Resolver: gatedResolver{inner: base.Resolver, paused: paused},
		Timeout:  base.Timeout,
	}
	inner := base.DialContext
	gated.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if paused() {
			return nil, errPaused
		}
		if inner != nil {
			return inner(ctx, network, address)
		}
		d := net.Dialer{Timeout: base.Timeout}
		if base.Timeout <= 0 {
			d.Timeout = 10 * time.Second
		}
		return d.DialContext(ctx, network, address)
	}
	return gated
}

// gatedResolver refuses resolution while the node is paused.
type gatedResolver struct {
	inner  tunnel.Resolver
	paused func() bool
}

func (g gatedResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	if g.paused != nil && g.paused() {
		return nil, errPaused
	}
	if g.inner == nil {
		return net.DefaultResolver.LookupIPAddr(ctx, host)
	}
	return g.inner.LookupIPAddr(ctx, host)
}

// backoffDelay returns the jittered delay for a reconnect attempt. The base
// doubles from min to max; jitter adds up to 50% on top so a fleet of nodes
// does not reconnect in lockstep, and the result never leaves [min, max].
func BackoffDelay(attempt int, min, max time.Duration, rnd func() float64) time.Duration {
	if min <= 0 {
		min = DefaultMinBackoff
	}
	if max < min {
		max = min
	}
	base := min
	for i := 0; i < attempt && base < max; i++ {
		if base > max/2 {
			base = max
			break
		}
		base *= 2
	}
	if base > max {
		base = max
	}
	if rnd == nil {
		rnd = rand.Float64
	}
	span := base / 2
	f := rnd()
	if f < 0 {
		f = 0
	}
	if f > 1 {
		f = 1
	}
	delay := base + time.Duration(f*float64(span))
	if delay > max {
		delay = max
	}
	return delay
}

// tunnelURL turns the configured hub base URL into the websocket tunnel
// endpoint, keeping any path prefix.
func TunnelURL(hubURL string) (string, error) {
	return hubEndpoint(hubURL, tunnelPath, map[string]string{
		"http":  "ws",
		"https": "wss",
		"ws":    "ws",
		"wss":   "wss",
	})
}

// httpURL turns the configured hub base URL into an HTTP endpoint for bind.
func HTTPURL(hubURL, path string) (string, error) {
	return hubEndpoint(hubURL, path, map[string]string{
		"http":  "http",
		"https": "https",
		"ws":    "http",
		"wss":   "https",
	})
}

func hubEndpoint(hubURL, path string, schemes map[string]string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(hubURL))
	if err != nil {
		return "", fmt.Errorf("parse hub URL: %w", err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("hub URL %q has no host", hubURL)
	}
	scheme, ok := schemes[strings.ToLower(u.Scheme)]
	if !ok {
		return "", fmt.Errorf("hub URL %q must use http(s) or ws(s)", hubURL)
	}
	if (scheme == "http" || scheme == "ws") && !loopbackHost(u.Hostname()) {
		return "", fmt.Errorf("hub URL %q must use https or wss unless the host is loopback", hubURL)
	}
	u.Scheme = scheme
	u.Path = strings.TrimSuffix(u.Path, "/") + path
	u.RawQuery = ""
	u.Fragment = ""
	u.User = nil
	return u.String(), nil
}

// loopbackHost reports whether host is localhost or a loopback IP literal
// (127.0.0.0/8, ::1). Only such hosts may use plaintext http/ws.
func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
