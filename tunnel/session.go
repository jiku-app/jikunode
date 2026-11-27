package tunnel

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"

	"github.com/jiku-app/jikunode/contract"
)

// signatureDomain separates tunnel proofs from every other ed25519 use.
var signatureDomain = []byte("jiku-tunnel-proof-v1:")

// SignatureMessage is the exact byte string a node signs:
// domain || hello nonce || challenge nonce. The proof is therefore bound to
// both fresh nonces and to this protocol.
func SignatureMessage(helloNonce, challengeNonce []byte) []byte {
	msg := make([]byte, 0, len(signatureDomain)+len(helloNonce)+len(challengeNonce))
	msg = append(msg, signatureDomain...)
	msg = append(msg, helloNonce...)
	msg = append(msg, challengeNonce...)
	return msg
}

// cryptoNonce returns NonceSize random bytes from crypto/rand.
func cryptoNonce() ([]byte, error) {
	buf := make([]byte, NonceSize)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("tunnel: entropy: %w", err)
	}
	return buf, nil
}

// ErrRejected is returned by ClientSession.Run when the hub refused the node's
// identity (unknown or revoked); reconnecting cannot succeed until the node is
// bound again.
var ErrRejected = errors.New("tunnel: rejected by hub")

// ErrDrained is returned by ClientSession.Run when the hub pushed Drain.
var ErrUpgraded = errors.New("tunnel: upgrade complete")

var ErrDrained = errors.New("tunnel: drained by hub")

// maxTrackedNonces bounds the replay memory: challenges older than the last
// maxTrackedNonces issues are forgotten (their proofs already fail signature
// verification because no live session holds that nonce).
const maxTrackedNonces = 4096

// NonceLog remembers every challenge nonce ever issued by one hub instance
// and refuses duplicates: a repeat means the entropy source is broken and the
// handshake stops (fail closed). This is the replay defence in depth — the
// primary defence is that each session verifies the signature over its own
// fresh nonces.
type NonceLog struct {
	mu    sync.Mutex
	seen  map[string]struct{}
	order []string
	rand  func() ([]byte, error)
}

// NewNonceLog returns a log using crypto/rand.
func NewNonceLog() *NonceLog { return NewNonceLogWithSource(nil) }

// NewNonceLogWithSource returns a log with a custom entropy source so the
// replay defence is testable. fn must return NonceSize random bytes; nil
// means crypto/rand.
func NewNonceLogWithSource(fn func() ([]byte, error)) *NonceLog {
	if fn == nil {
		fn = cryptoNonce
	}
	return &NonceLog{seen: map[string]struct{}{}, rand: fn}
}

// Issue returns a nonce this log has never issued before.
func (l *NonceLog) Issue() ([]byte, error) {
	n, err := l.rand()
	if err != nil {
		return nil, fmt.Errorf("tunnel: nonce entropy: %w", err)
	}
	if len(n) != NonceSize {
		return nil, errors.New("tunnel: nonce source returned wrong size")
	}
	key := string(n)
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, dup := l.seen[key]; dup {
		return nil, errors.New("tunnel: nonce source repeated a challenge; refusing handshake")
	}
	l.seen[key] = struct{}{}
	l.order = append(l.order, key)
	if len(l.order) > maxTrackedNonces {
		delete(l.seen, l.order[0])
		l.order = l.order[1:]
	}
	return n, nil
}

// MuxConfig returns the shared yamux configuration. yamux's own logging is
// discarded so nothing from the multiplexer can reach stdout (OBS S10).
func MuxConfig() *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	cfg.EnableKeepAlive = false
	return cfg
}

// nopMetrics is used when no metrics are injected.
type nopMetrics struct{}

func (nopMetrics) SessionDelta(int)           {}
func (nopMetrics) Reconnect()                 {}
func (nopMetrics) LeaseExpired()              {}
func (nopMetrics) HeartbeatRTT(time.Duration) {}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// ---- hub side ----------------------------------------------------------------

// ServerSession is the hub side of one node tunnel.
type ServerSession struct {
	ys        *yamux.Session
	nonces    *NonceLog
	mu        sync.Mutex // serializes control-stream writes
	ctrl      *yamux.Stream
	hello     *Hello
	challenge []byte
}

// NewServerSession wraps a yamux server session. nonces may be nil (a private
// log is created); sharing one log across all sessions of a hub instance is
// the intended production use because replay defence must span sessions.
func NewServerSession(ys *yamux.Session, nonces *NonceLog) *ServerSession {
	if nonces == nil {
		nonces = NewNonceLog()
	}
	return &ServerSession{ys: ys, nonces: nonces}
}

// AcceptControl accepts the first stream (the control stream) and reads the
// Hello within timeout. Anything else first is a protocol error.
func (s *ServerSession) AcceptControl(ctx context.Context, timeout time.Duration) (*Hello, error) {
	ctrl, err := s.ys.AcceptStreamWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("tunnel: accept control stream: %w", err)
	}
	s.ctrl = ctrl
	ctrl.SetReadDeadline(time.Now().Add(timeout))
	c, err := ReadControl(ctrl)
	ctrl.SetReadDeadline(time.Time{})
	if err != nil {
		return nil, fmt.Errorf("tunnel: read hello: %w", err)
	}
	if c.Hello == nil {
		return nil, errors.New("tunnel: first control message must be hello")
	}
	if c.Hello.HeartbeatSeconds < 5 || c.Hello.HeartbeatSeconds > 300 {
		return nil, errors.New("tunnel: heartbeat interval must be 5..300 seconds")
	}
	s.hello = c.Hello
	return c.Hello, nil
}

// SendChallenge issues a fresh 32-byte nonce (crypto/rand via the NonceLog)
// and sends the Challenge.
func (s *ServerSession) SendChallenge() error {
	n, err := s.nonces.Issue()
	if err != nil {
		return err
	}
	s.challenge = n
	s.mu.Lock()
	defer s.mu.Unlock()
	return WriteControl(s.ctrl, &Control{Challenge: &Challenge{Nonce: n}})
}

// ReadProof reads the Proof within timeout.
func (s *ServerSession) ReadProof(timeout time.Duration) (*Proof, error) {
	s.ctrl.SetReadDeadline(time.Now().Add(timeout))
	c, err := ReadControl(s.ctrl)
	s.ctrl.SetReadDeadline(time.Time{})
	if err != nil {
		return nil, fmt.Errorf("tunnel: read proof: %w", err)
	}
	if c.Proof == nil {
		return nil, errors.New("tunnel: expected proof after challenge")
	}
	return c.Proof, nil
}

// VerifyProof checks the ed25519 signature over domain||hello||challenge
// with strict input sizes.
func (s *ServerSession) VerifyProof(pub ed25519.PublicKey, p *Proof) bool {
	if len(pub) != ed25519.PublicKeySize || p == nil || len(p.Signature) != ed25519.SignatureSize {
		return false
	}
	if s.hello == nil || len(s.hello.Nonce) != NonceSize || len(s.challenge) != NonceSize {
		return false
	}
	return ed25519.Verify(pub, SignatureMessage(s.hello.Nonce, s.challenge), p.Signature)
}

// SendAck acknowledges a completed handshake (a Heartbeat doubles as the ack;
// no other message type exists for it).
func (s *ServerSession) SendAck() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return WriteControl(s.ctrl, &Control{Heartbeat: &Heartbeat{}})
}

// SendDesiredState sends the hub's authoritative run state. It is sent once
// the session is authenticated and again whenever the stored state changes.
func (s *ServerSession) SendDesiredState(st contract.RunState) error {
	if !ValidRunState(st) {
		return fmt.Errorf("tunnel: invalid run state %q", st)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return WriteControl(s.ctrl, &Control{DesiredState: &DesiredState{State: st}})
}

// ControlHandler configures ServeControl.
type ControlHandler struct {
	OnUpgradeStatus func(UpgradeStatus)
	// HeartbeatTimeout overrides the declared interval's 3x timeout for tests.
	HeartbeatTimeout time.Duration
	// OnHeartbeat receives the elapsed time between hub send and node echo.
	OnHeartbeat func(time.Duration)
	// OnSetState applies a node-initiated pause/run; an error fails the
	// session so the node reconnects and retries instead of believing a
	// state change that was never persisted.
	OnSetState func(contract.RunState) error
}

// maxPendingHeartbeats bounds outstanding unechoed heartbeats per session.
const maxPendingHeartbeats = 3

// ServeControl is the hub's control read loop: it fails the session when no
// heartbeat arrives within HeartbeatTimeout, applies SetState through
// OnSetState, and rejects every other message as a protocol violation.
func (s *ServerSession) ServeControl(h ControlHandler) error {
	interval := time.Duration(s.hello.HeartbeatSeconds) * time.Second
	if h.HeartbeatTimeout <= 0 {
		h.HeartbeatTimeout = 3 * interval
	}
	stop := make(chan struct{})
	defer close(stop)
	// Send times of heartbeats not yet echoed, oldest first. A node that was
	// asleep answers every queued heartbeat on wake-up, so each echo consumes
	// the oldest outstanding send. The queue is capped at maxPendingHeartbeats:
	// a node whose echoes lag by more than that many intervals fails the
	// session, which is the 3x-interval contract (HeartbeatTimeout may be set
	// larger, but never lets the queue grow past the cap).
	var (
		pendMu  sync.Mutex
		pending []time.Time
	)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				s.mu.Lock()
				pendMu.Lock()
				if len(pending) >= maxPendingHeartbeats {
					pendMu.Unlock()
					s.mu.Unlock()
					_ = s.Close() // echoes lag by more than the cap: fail the session
					return
				}
				pending = append(pending, time.Now())
				pendMu.Unlock()
				err := WriteControl(s.ctrl, &Control{Heartbeat: &Heartbeat{}})
				s.mu.Unlock()
				if err != nil {
					_ = s.Close()
					return
				}
			}
		}
	}()
	deadline := time.Now().Add(h.HeartbeatTimeout)
	for {
		s.ctrl.SetReadDeadline(deadline)
		c, err := ReadControl(s.ctrl)
		s.ctrl.SetReadDeadline(time.Time{})
		if err != nil {
			return fmt.Errorf("tunnel: control read: %w", err)
		}
		switch {
		case c.Heartbeat != nil:
			pendMu.Lock()
			if len(pending) == 0 {
				pendMu.Unlock()
				return errors.New("tunnel: unsolicited heartbeat echo")
			}
			when := pending[0]
			pending = pending[1:]
			pendMu.Unlock()
			if h.OnHeartbeat != nil {
				h.OnHeartbeat(time.Since(when))
			}
			deadline = time.Now().Add(h.HeartbeatTimeout)
		case c.UpgradeStatus != nil:
			if h.OnUpgradeStatus != nil {
				h.OnUpgradeStatus(*c.UpgradeStatus)
			}
		case c.SetState != nil:
			if !ValidRunState(c.SetState.State) {
				return fmt.Errorf("tunnel: invalid run state %q", c.SetState.State)
			}
			if err := h.OnSetState(c.SetState.State); err != nil {
				return fmt.Errorf("tunnel: set state: %w", err)
			}
		default:
			return errors.New("tunnel: unexpected control message")
		}
	}
}

// openStreamCtx opens a yamux stream or fails when ctx is done first.
func openStreamCtx(ys *yamux.Session, ctx context.Context) (*yamux.Stream, error) {
	type res struct {
		stream *yamux.Stream
		err    error
	}
	ch := make(chan res)
	go func() {
		st, err := ys.OpenStream()
		select {
		case ch <- res{st, err}:
		case <-ctx.Done():
			if st != nil {
				_ = st.Close()
			}
		}
	}()
	select {
	case r := <-ch:
		return r.stream, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

const streamDialTimeout = 5 * time.Second

const (
	dialOK byte = iota
	dialError
)

// DialCode is the wire code a node returns when it refuses a stream dial.
type DialCode byte

const (
	// DialRefused: the destination is a refused (non-public) address.
	DialRefused DialCode = iota + 1
	// DialFailed: the dial failed (resolver error, refused, unreachable).
	DialFailed
	// DialTimeout: the dial timed out.
	DialTimeout
	// DialNoIPv6: a seat dial found no IPv6 address for the target.
	DialNoIPv6
	// DialSeatRefused: the requested seat source is not inside this node's pool.
	DialSeatRefused
)

// DialRejectedError is returned when the node refused the dial. It wraps
// contract.ErrNodeDialFailed, so every existing egress classification still
// applies, and carries the wire code so a caller can tell a permanent
// condition (DialNoIPv6, DialSeatRefused) from a transport failure.
type DialRejectedError struct{ Code DialCode }

func (e *DialRejectedError) Error() string {
	return fmt.Sprintf("node rejected dial (code %d)", byte(e.Code))
}

// Unwrap exposes contract.ErrNodeDialFailed to errors.Is.
func (e *DialRejectedError) Unwrap() error { return contract.ErrNodeDialFailed }

// OpenStream opens a data stream and writes the stream header. The target
// host travels the wire but is never logged. ctx bounds the open: yamux has
// no context-aware open, so the open is raced against ctx; if ctx wins the
// late open (if any) is closed when it completes.
func (s *ServerSession) OpenStream(ctx context.Context, host string, port int) (net.Conn, error) {
	return s.openStream(ctx, &StreamHeader{V: ProtocolVersion, TargetHost: host, TargetPort: port})
}

// OpenStreamFrom opens a seat-addressed data stream: the node must egress from
// source, an address inside one of its configured pool prefixes. A node that
// did not advertise CapabilitySeatDial rejects it with DialSeatRefused, which
// is why the caller must check the hello capabilities first.
func (s *ServerSession) OpenStreamFrom(ctx context.Context, host string, port int, source net.IP) (net.Conn, error) {
	if source == nil || source.To4() != nil || source.IsUnspecified() {
		return nil, fmt.Errorf("%w: seat source must be an IPv6 address", contract.ErrNodeDialFailed)
	}
	return s.openStream(ctx, &StreamHeader{V: ProtocolVersionSeat, TargetHost: host, TargetPort: port, SourceIPv6: source.String()})
}

func (s *ServerSession) openStream(ctx context.Context, hdr *StreamHeader) (net.Conn, error) {
	if err := hdr.Validate(); err != nil {
		return nil, err
	}
	stream, err := openStreamCtx(s.ys, ctx)
	if err != nil {
		return nil, fmt.Errorf("tunnel: open stream: %w", err)
	}
	stream.SetWriteDeadline(time.Now().Add(DefaultHeaderTimeout))
	err = writeFrame(stream, hdr)
	stream.SetWriteDeadline(time.Time{})
	if err != nil {
		stream.Close()
		return nil, fmt.Errorf("tunnel: write stream header: %w", err)
	}
	deadline := time.Now().Add(streamDialTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = stream.SetReadDeadline(deadline)
	callbackDone := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(callbackDone)
		_ = stream.SetReadDeadline(time.Now())
	})
	var status [1]byte
	_, err = io.ReadFull(stream, status[:])
	if err == nil && status[0] == dialError {
		var code [1]byte
		_, err = io.ReadFull(stream, code[:])
		if err == nil {
			err = &DialRejectedError{Code: DialCode(code[0])}
		}
	} else if err == nil && status[0] != dialOK {
		err = errors.New("invalid dial response")
	}
	if !stop() {
		<-callbackDone
	}
	_ = stream.SetReadDeadline(time.Time{})
	if err != nil {
		_ = stream.Close()
		return nil, fmt.Errorf("%w: stream dial response: %w", contract.ErrNodeDialFailed, err)
	}
	return stream, nil
}

// SendReject tells the node its identity is refused for good (see Reject).
func (s *ServerSession) SendReject(reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return WriteControl(s.ctrl, &Control{Reject: &Reject{Reason: reason}})
}

// SendDrain pushes Drain to the node.
func (s *ServerSession) SendDrain(reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return WriteControl(s.ctrl, &Control{Drain: &Drain{Reason: reason}})
}

// Close closes the multiplexed session (and therefore every stream).
func (s *ServerSession) Close() error { return s.ys.Close() }

// ---- node side ---------------------------------------------------------------

// ClientConfig configures the node side of a tunnel.
type ClientConfig struct {
	OnUpgrade func(context.Context, string, func(string) error) error
	// OnDesiredState receives the hub's authoritative run state (on connect
	// and whenever it changes). It is the node's only pause/resume source.
	OnDesiredState    func(contract.RunState)
	NodeID            string
	Version           string
	PrivateKey        ed25519.PrivateKey
	Policy            *DialPolicy            // nil => DefaultDialPolicy
	HeartbeatInterval time.Duration          // 0 => 10 s
	HeaderTimeout     time.Duration          // 0 => 10 s (data-stream header read)
	ChallengeTimeout  time.Duration          // 0 => 10 s (challenge and ack reads)
	Metrics           contract.TunnelMetrics // nil-safe
	Logger            *slog.Logger           // nil => discard
	// PoolPrefixes lists the IPv6 prefixes this node may egress from when the
	// hub asks for a seat (a ProtocolVersionSeat stream header). Empty means
	// this is not a pool node: it advertises no CapabilitySeatDial and refuses
	// every seat dial, so a compromised hub cannot make it spoof an address.
	PoolPrefixes []netip.Prefix
}

// ClientSession is the node side of one tunnel connection.
type ClientSession struct {
	cfg  ClientConfig
	ys   *yamux.Session
	ctrl *yamux.Stream
	mu   sync.Mutex // serializes control-stream writes
}

// NewClientSession validates the node-side configuration.
func NewClientSession(cfg ClientConfig) (*ClientSession, error) {
	if cfg.NodeID == "" {
		return nil, errors.New("tunnel: node id required")
	}
	if len(cfg.PrivateKey) != ed25519.PrivateKeySize {
		return nil, errors.New("tunnel: ed25519 private key required")
	}
	if cfg.HeartbeatInterval == 0 {
		cfg.HeartbeatInterval = DefaultHeartbeatInterval
	}
	if cfg.HeartbeatInterval%time.Second != 0 || cfg.HeartbeatInterval < 5*time.Second || cfg.HeartbeatInterval > 300*time.Second {
		return nil, errors.New("tunnel: heartbeat interval must be 5..300 whole seconds")
	}
	return &ClientSession{cfg: cfg}, nil
}

func (s *ClientSession) policy() *DialPolicy {
	if s.cfg.Policy == nil {
		return DefaultDialPolicy()
	}
	return s.cfg.Policy
}

func (s *ClientSession) heartbeatInterval() time.Duration {
	if s.cfg.HeartbeatInterval <= 0 {
		return DefaultHeartbeatInterval
	}
	return s.cfg.HeartbeatInterval
}

func (s *ClientSession) headerTimeout() time.Duration {
	if s.cfg.HeaderTimeout <= 0 {
		return DefaultHeaderTimeout
	}
	return s.cfg.HeaderTimeout
}

func (s *ClientSession) challengeTimeout() time.Duration {
	if s.cfg.ChallengeTimeout <= 0 {
		return DefaultChallengeTimeout
	}
	return s.cfg.ChallengeTimeout
}

func (s *ClientSession) metrics() contract.TunnelMetrics {
	if s.cfg.Metrics == nil {
		return nopMetrics{}
	}
	return s.cfg.Metrics
}

func (s *ClientSession) logger() *slog.Logger {
	if s.cfg.Logger == nil {
		return discardLogger()
	}
	return s.cfg.Logger
}

// Run drives the node side over an established websocket: handshake, then
// heartbeats and data streams until the session fails, the hub drains it or
// ctx is done.
func (s *ClientSession) Run(ctx context.Context, ws *websocket.Conn) error {
	nc := websocket.NetConn(ctx, ws, websocket.MessageBinary)
	ys, err := yamux.Client(nc, MuxConfig())
	if err != nil {
		_ = ws.Close(websocket.StatusInternalError, "")
		return fmt.Errorf("tunnel: yamux client: %w", err)
	}
	s.ys = ys
	defer func() {
		_ = ys.Close()
		_ = ws.Close(websocket.StatusNormalClosure, "")
	}()

	if err := s.handshake(); err != nil {
		return err
	}
	s.logger().Info("tunnel: session established", "event", "session_established", "node_id", s.cfg.NodeID)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	drainCh := make(chan error, 1)
	upgradeDone := make(chan error, 1)
	go func() { drainCh <- s.watchDrain(runCtx, upgradeDone) }()
	serveCh := make(chan error, 1)
	go func() { serveCh <- s.serveStreams(runCtx) }()

	select {
	case err := <-upgradeDone:
		return err
	case err := <-drainCh:
		cancel()
		_ = ys.Close() // unblocks serveStreams
		<-serveCh
		return err
	case err := <-serveCh:
		cancel()
		return err
	case <-runCtx.Done():
		return ctx.Err()
	}
}

// handshake opens the control stream and completes the challenge exchange.
func (s *ClientSession) handshake() error {
	ctrl, err := s.ys.OpenStream()
	if err != nil {
		return fmt.Errorf("tunnel: open control stream: %w", err)
	}
	s.ctrl = ctrl

	nonce, err := cryptoNonce()
	if err != nil {
		return err
	}
	if err := WriteControl(ctrl, &Control{Hello: &Hello{
		NodeID: s.cfg.NodeID, Version: s.cfg.Version, Nonce: nonce, HeartbeatSeconds: int(s.heartbeatInterval() / time.Second),
		Capabilities: s.capabilities(),
	}}); err != nil {
		return fmt.Errorf("tunnel: send hello: %w", err)
	}

	timeout := s.challengeTimeout()
	ctrl.SetReadDeadline(time.Now().Add(timeout))
	c, err := ReadControl(ctrl)
	ctrl.SetReadDeadline(time.Time{})
	if err != nil {
		return fmt.Errorf("tunnel: read challenge: %w", err)
	}
	if c.Reject != nil {
		return fmt.Errorf("%w: %s", ErrRejected, c.Reject.Reason)
	}
	if c.Challenge == nil || len(c.Challenge.Nonce) != NonceSize {
		return errors.New("tunnel: expected 32-byte challenge")
	}

	sig := ed25519.Sign(s.cfg.PrivateKey, SignatureMessage(nonce, c.Challenge.Nonce))
	if err := WriteControl(ctrl, &Control{Proof: &Proof{Signature: sig}}); err != nil {
		return fmt.Errorf("tunnel: send proof: %w", err)
	}

	ctrl.SetReadDeadline(time.Now().Add(timeout))
	c, err = ReadControl(ctrl)
	ctrl.SetReadDeadline(time.Time{})
	if err != nil {
		return fmt.Errorf("tunnel: read ack: %w", err)
	}
	if c.Heartbeat == nil {
		return errors.New("tunnel: expected handshake ack")
	}
	return nil
}

// watchDrain echoes hub heartbeats until the hub drains or rejects the session
// or the stream fails. Other messages are protocol violations.
func (s *ClientSession) watchDrain(ctx context.Context, done chan<- error) error {
	attempted := false
	for {
		s.ctrl.SetReadDeadline(time.Now().Add(3 * s.heartbeatInterval()))
		c, err := ReadControl(s.ctrl)
		if err != nil {
			return fmt.Errorf("tunnel: control read: %w", err)
		}
		if c.Heartbeat != nil {
			if err := s.writeControl(&Control{Heartbeat: &Heartbeat{}}); err != nil {
				return fmt.Errorf("tunnel: heartbeat echo: %w", err)
			}
			continue
		}
		if c.DesiredState != nil {
			if !ValidRunState(c.DesiredState.State) {
				return fmt.Errorf("tunnel: invalid run state %q", c.DesiredState.State)
			}
			if s.cfg.OnDesiredState != nil {
				s.cfg.OnDesiredState(c.DesiredState.State)
			}
			continue
		}
		if c.Upgrade != nil {
			version := c.Upgrade.Version
			status := func(state string) error {
				return s.writeControl(&Control{UpgradeStatus: &UpgradeStatus{Version: version, State: state}})
			}
			if attempted {
				continue
			}
			attempted = true
			if s.cfg.OnUpgrade == nil {
				if err := status("unsupported"); err != nil {
					return err
				}
				continue
			}
			go func() {
				if err := s.cfg.OnUpgrade(ctx, version, status); err != nil {
					done <- err
				}
			}()
			continue
		}
		if c.Reject != nil {
			return fmt.Errorf("%w: %s", ErrRejected, c.Reject.Reason)
		}
		if c.Drain == nil {
			return errors.New("tunnel: unexpected control message")
		}
		return fmt.Errorf("%w: %s", ErrDrained, c.Drain.Reason)
	}
}

// writeControl serializes control-stream writes (heartbeats and SetState can
// race).
func (s *ClientSession) writeControl(c *Control) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return WriteControl(s.ctrl, c)
}

// SendSetState pushes a node-initiated pause/run. Safe to call while Run is
// serving.
func (s *ClientSession) SendSetState(st contract.RunState) error {
	if !ValidRunState(st) {
		return fmt.Errorf("tunnel: invalid run state %q", st)
	}
	return s.writeControl(&Control{SetState: &SetState{State: st}})
}

// serveStreams accepts hub-opened data streams forever.
func (s *ClientSession) serveStreams(ctx context.Context) error {
	for {
		stream, err := s.ys.AcceptStream()
		if err != nil {
			return fmt.Errorf("tunnel: accept stream: %w", err)
		}
		go s.handleStream(ctx, stream)
	}
}

// handleStream reads one stream header (10 s deadline), dials the target
// through the DialPolicy and copies bytes both ways with half-close
// propagation. A seat-addressed header additionally requires a source inside
// this node's pool prefixes. On any error the stream is closed.
func (s *ClientSession) handleStream(ctx context.Context, stream *yamux.Stream) {
	hdr, err := readStreamHeader(stream, s.headerTimeout())
	if err != nil {
		s.logger().Info("tunnel: stream header rejected", "event", "stream_header_rejected", "node_id", s.cfg.NodeID)
		_ = stream.Close()
		return
	}
	var source net.IP
	if hdr.V == ProtocolVersionSeat {
		source, err = hdr.Source()
		if err == nil && !s.seatAllowed(source) {
			s.logger().Info("tunnel: seat refused", "event", "seat_refused", "node_id", s.cfg.NodeID)
			s.rejectStream(stream, DialSeatRefused)
			return
		}
		if err != nil {
			s.logger().Info("tunnel: stream header rejected", "event", "stream_header_rejected", "node_id", s.cfg.NodeID)
			_ = stream.Close()
			return
		}
	}
	dialCtx, cancel := context.WithTimeout(ctx, streamDialTimeout)
	defer cancel()
	conn, err := s.dialStream(dialCtx, hdr, source)
	if err != nil {
		s.logger().Info("tunnel: target refused", "event", "target_refused", "node_id", s.cfg.NodeID)
		code := DialFailed
		var refused *RefusedAddrError
		switch {
		case errors.As(err, &refused):
			code = DialRefused
		case errors.Is(err, ErrNoIPv6):
			code = DialNoIPv6
		case errors.Is(err, context.DeadlineExceeded):
			code = DialTimeout
		}
		s.rejectStream(stream, code)
		return
	}
	_ = stream.SetWriteDeadline(time.Now().Add(streamDialTimeout))
	_, err = stream.Write([]byte{dialOK})
	_ = stream.SetWriteDeadline(time.Time{})
	if err != nil {
		_ = conn.Close()
		_ = stream.Close()
		return
	}
	relay(s.ys.CloseChan(), stream, conn)
}

// dialStream dials the header target, binding the seat source when the header
// is seat-addressed.
func (s *ClientSession) dialStream(ctx context.Context, hdr *StreamHeader, source net.IP) (net.Conn, error) {
	policy := s.policy()
	if source == nil {
		return policy.Dial(ctx, hdr.TargetHost, hdr.TargetPort)
	}
	return policy.DialFrom(ctx, hdr.TargetHost, hdr.TargetPort, source)
}

// rejectStream writes the dial-refused response and closes the stream.
func (s *ClientSession) rejectStream(stream *yamux.Stream, code DialCode) {
	_ = stream.SetWriteDeadline(time.Now().Add(streamDialTimeout))
	_, _ = stream.Write([]byte{dialError, byte(code)})
	_ = stream.Close()
}

// capabilities reports the wire capabilities this node advertises in Hello.
func (s *ClientSession) capabilities() []string {
	if len(s.cfg.PoolPrefixes) == 0 {
		return nil
	}
	return []string{CapabilitySeatDial}
}

// seatAllowed reports whether src is inside one of the configured pool
// prefixes. It is the node-side half of the seat invariant: a hub can pick a
// seat address, but never an address this node was not told to use.
func (s *ClientSession) seatAllowed(src net.IP) bool {
	addr, ok := netip.AddrFromSlice(src)
	if !ok {
		return false
	}
	addr = addr.Unmap()
	for _, p := range s.cfg.PoolPrefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// readStreamHeader reads one framed stream header within timeout; V must be 1.
func readStreamHeader(stream *yamux.Stream, timeout time.Duration) (*StreamHeader, error) {
	if timeout <= 0 {
		timeout = DefaultHeaderTimeout
	}
	stream.SetReadDeadline(time.Now().Add(timeout))
	var h StreamHeader
	err := readFrame(stream, &h)
	stream.SetReadDeadline(time.Time{})
	if err != nil {
		return nil, fmt.Errorf("tunnel: read stream header: %w", err)
	}
	if err := h.Validate(); err != nil {
		return nil, err
	}
	return &h, nil
}

// relay copies bytes between a yamux stream and a dialed target with proper
// half-closes: EOF on the target closes the stream's write side (yamux Close
// sends a FIN while reads continue), and EOF from the hub half-closes the
// target. done (the session's close channel) force-closes the target so a
// silent peer can never pin a goroutine past the session's lifetime; both
// directions ending closes the target fully.
func relay(done <-chan struct{}, stream *yamux.Stream, target net.Conn) {
	finished := make(chan struct{}, 2)
	hubFinished := make(chan struct{})
	go func() {
		_, _ = io.Copy(stream, target) // target -> stream
		_ = stream.Close()             // FIN: hub reads EOF after draining
		finished <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(target, stream) // stream -> target
		closeWrite(target)
		close(hubFinished)
		finished <- struct{}{}
	}()

	both := make(chan struct{})
	go func() {
		<-finished
		<-finished
		close(both)
	}()
	select {
	case <-both:
	case <-done:
		_ = target.Close()
		_ = stream.Close()
		<-both
	case <-hubFinished:
		timer := time.NewTimer(2 * time.Second)
		defer timer.Stop()
		select {
		case <-both:
		case <-done:
			_ = target.Close()
			_ = stream.Close()
			<-both
		case <-timer.C:
			_ = target.Close()
			_ = stream.Close()
			<-both
		}
	}
	_ = target.Close()
}

// closeWrite half-closes a connection when supported, else closes it.
func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}

func (s *ServerSession) SendUpgrade(version string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return WriteControl(s.ctrl, &Control{Upgrade: &Upgrade{Version: version}})
}
