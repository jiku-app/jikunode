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
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"

	"github.com/jiku-app/jikunode/internal/contract"
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

// ErrDrained is returned by ClientSession.Run when the hub pushed Drain.
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

// ControlHandler configures ServeControl.
type ControlHandler struct {
	// HeartbeatTimeout bounds the wait between heartbeats (0 => 30 s).
	HeartbeatTimeout time.Duration
	// OnHeartbeat runs after every accepted heartbeat (RTT sampling).
	OnHeartbeat func()
	// OnSetState applies a node-initiated pause/run; an error fails the
	// session so the node reconnects and retries instead of believing a
	// state change that was never persisted.
	OnSetState func(contract.RunState) error
}

// ServeControl is the hub's control read loop: it fails the session when no
// heartbeat arrives within HeartbeatTimeout, applies SetState through
// OnSetState, and rejects every other message as a protocol violation.
func (s *ServerSession) ServeControl(h ControlHandler) error {
	if h.HeartbeatTimeout <= 0 {
		h.HeartbeatTimeout = DefaultHeartbeatTimeout
	}
	for {
		s.ctrl.SetReadDeadline(time.Now().Add(h.HeartbeatTimeout))
		c, err := ReadControl(s.ctrl)
		s.ctrl.SetReadDeadline(time.Time{})
		if err != nil {
			return fmt.Errorf("tunnel: control read: %w", err)
		}
		switch {
		case c.Heartbeat != nil:
			if h.OnHeartbeat != nil {
				h.OnHeartbeat()
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

// Ping measures the multiplexer round trip; used for heartbeat RTT metrics.
func (s *ServerSession) Ping(timeout time.Duration) (time.Duration, error) {
	done := make(chan pingResult, 1)
	go func() {
		d, err := s.ys.Ping()
		done <- pingResult{d: d, err: err}
	}()
	select {
	case r := <-done:
		return r.d, r.err
	case <-time.After(timeout):
		return 0, errors.New("tunnel: ping timeout")
	}
}

type pingResult struct {
	d   time.Duration
	err error
}

const streamDialTimeout = 5 * time.Second

const (
	dialOK byte = iota
	dialError
)
const (
	dialRefused byte = iota + 1
	dialFailed
	dialTimeout
)

// OpenStream opens a data stream and writes the stream header. The target
// host travels the wire but is never logged. ctx bounds the open: yamux has
// no context-aware open, so the open is raced against ctx; if ctx wins the
// late open (if any) is closed when it completes.
func (s *ServerSession) OpenStream(ctx context.Context, host string, port int) (net.Conn, error) {
	hdr := &StreamHeader{V: ProtocolVersion, TargetHost: host, TargetPort: port}
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
			err = fmt.Errorf("node rejected dial (code %d)", code[0])
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
		return nil, fmt.Errorf("%w: stream dial response: %v", contract.ErrNodeDialFailed, err)
	}
	return stream, nil
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
	NodeID            string
	Version           string
	PrivateKey        ed25519.PrivateKey
	Policy            *DialPolicy            // nil => DefaultDialPolicy
	HeartbeatInterval time.Duration          // 0 => 10 s
	HeaderTimeout     time.Duration          // 0 => 10 s (data-stream header read)
	ChallengeTimeout  time.Duration          // 0 => 10 s (challenge and ack reads)
	Metrics           contract.TunnelMetrics // nil-safe
	Logger            *slog.Logger           // nil => discard
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
	go func() { drainCh <- s.watchDrain() }()
	hbCh := make(chan error, 1)
	go func() { hbCh <- s.heartbeats(runCtx) }()
	serveCh := make(chan error, 1)
	go func() { serveCh <- s.serveStreams(runCtx) }()

	select {
	case err := <-drainCh:
		cancel()
		_ = ys.Close() // unblocks serveStreams
		<-serveCh
		return err
	case err := <-hbCh:
		cancel()
		_ = ys.Close()
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
		NodeID: s.cfg.NodeID, Version: s.cfg.Version, Nonce: nonce,
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

// watchDrain reads the control stream until the hub drains the session or the
// stream fails. Any message other than Drain is a protocol violation.
func (s *ClientSession) watchDrain() error {
	for {
		c, err := ReadControl(s.ctrl)
		if err != nil {
			return fmt.Errorf("tunnel: control read: %w", err)
		}
		if c.Drain == nil {
			return errors.New("tunnel: unexpected control message")
		}
		return fmt.Errorf("%w: %s", ErrDrained, c.Drain.Reason)
	}
}

// heartbeats sends Heartbeat until ctx is done or a write fails.
func (s *ClientSession) heartbeats(ctx context.Context) error {
	t := time.NewTicker(s.heartbeatInterval())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if err := s.writeControl(&Control{Heartbeat: &Heartbeat{}}); err != nil {
				return fmt.Errorf("tunnel: heartbeat: %w", err)
			}
		}
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

// handleStream reads one stream header (10 s deadline, version 1), dials the
// target through the DialPolicy and copies bytes both ways with half-close
// propagation. On any error the stream is closed.
func (s *ClientSession) handleStream(ctx context.Context, stream *yamux.Stream) {
	hdr, err := readStreamHeader(stream, s.headerTimeout())
	if err != nil {
		s.logger().Info("tunnel: stream header rejected", "event", "stream_header_rejected", "node_id", s.cfg.NodeID)
		_ = stream.Close()
		return
	}
	dialCtx, cancel := context.WithTimeout(ctx, streamDialTimeout)
	defer cancel()
	conn, err := s.policy().Dial(dialCtx, hdr.TargetHost, hdr.TargetPort)
	if err != nil {
		s.logger().Info("tunnel: target refused", "event", "target_refused", "node_id", s.cfg.NodeID)
		code := dialFailed
		var refused *RefusedAddrError
		if errors.As(err, &refused) {
			code = dialRefused
		} else if errors.Is(err, context.DeadlineExceeded) {
			code = dialTimeout
		}
		_ = stream.SetWriteDeadline(time.Now().Add(streamDialTimeout))
		_, _ = stream.Write([]byte{dialError, code})
		_ = stream.Close()
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
