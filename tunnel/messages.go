// Package tunnel implements the jiku node-tunnel wire protocol: a
// length-prefixed JSON control stream and {v,target_host,target_port} framed
// data-stream headers multiplexed by yamux, plus the node-side destination
// policy (post-DNS refusal of every non-public address).
//
// Security notes: control frames are capped at 64 KiB and the cap is enforced
// BEFORE any buffer is allocated, so a hostile peer cannot force the reader to
// allocate; data-stream headers must arrive within a deadline and must carry
// protocol version 1.
package tunnel

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	"github.com/jiku-app/jikunode/contract"
)

// Wire constants. Timeouts are conservative: a peer that stalls any framing
// step fails the session instead of pinning resources.
const (
	// ProtocolVersion is the plain data-stream header: the node dials the
	// target from its own primary address.
	ProtocolVersion = 1

	// ProtocolVersionSeat is the seat-addressed data-stream header: it names
	// the IPv6 source the node must egress from (a Jiku egress-pool seat).
	// Only a node that advertises CapabilitySeatDial may receive it.
	ProtocolVersionSeat = 2

	// CapabilitySeatDial is advertised in Hello by a node configured with pool
	// prefixes (see ClientConfig.PoolPrefixes). A hub must not send a
	// ProtocolVersionSeat header to a node that did not advertise it.
	CapabilitySeatDial = "seat-dial"

	// MaxControlMessageSize bounds one framed control message (or stream
	// header). Rejected before allocation.
	MaxControlMessageSize = 64 * 1024

	// DefaultHeaderTimeout bounds a data-stream header read (node side).
	DefaultHeaderTimeout = 10 * time.Second

	// DefaultHelloTimeout bounds the hub's first Hello read.
	DefaultHelloTimeout = 5 * time.Second

	// DefaultProofTimeout bounds the hub's Proof read after the challenge.
	DefaultProofTimeout = 10 * time.Second

	// DefaultChallengeTimeout bounds the node's Challenge read.
	DefaultChallengeTimeout = 10 * time.Second

	// DefaultHeartbeatInterval is the node's declared hub heartbeat cadence.
	DefaultHeartbeatInterval = 10 * time.Second

	// NonceSize is the size of both the Hello nonce and the Challenge nonce.
	NonceSize = 32
)

// Hello is the node's first control message: its node id, agent version and
// a fresh nonce that the proof signs together with the hub challenge. The hub
// looks the node up (it must already be bound); the key and name were
// registered at bind time, never at connect time.
type Hello struct {
	NodeID           string   `json:"node_id"`
	Version          string   `json:"version"`
	Nonce            []byte   `json:"nonce"`
	HeartbeatSeconds int      `json:"heartbeat_seconds"`
	Capabilities     []string `json:"capabilities,omitempty"`
}

// HasCapability reports whether the hello advertises c. An unknown capability
// is ignored, never an error.
func (h *Hello) HasCapability(c string) bool {
	if h == nil {
		return false
	}
	for _, got := range h.Capabilities {
		if got == c {
			return true
		}
	}
	return false
}

// Challenge carries the hub's 32 random bytes (crypto/rand).
type Challenge struct {
	Nonce []byte `json:"nonce"`
}

// Proof carries the node's ed25519 signature over SignatureMessage.
type Proof struct {
	Signature []byte `json:"signature"`
}

// Reject reasons: the hub does not recognise the node, so reconnecting with
// the same identity can never succeed.
const (
	// RejectUnknownNode means the hub has no such node (never bound, or its
	// data was reset).
	RejectUnknownNode = "unknown_node"
	// RejectRevokedNode means the node was revoked by its owner or an admin.
	RejectRevokedNode = "revoked_node"
)

// Reject tells the node the hub refuses its identity for good. The node must
// stop reconnecting and wait for a new bind.
type Reject struct {
	Reason string `json:"reason"`
}

// Heartbeat{} keeps the session lease alive; the hub also uses it as the
// handshake-success acknowledgement.
type Heartbeat struct{}

// SetState is a node-initiated pause/run request. The hub decides: it stores
// the state and pushes the result back as DesiredState.
type SetState struct {
	State contract.RunState `json:"state"`
}

// DesiredState is the hub's authoritative pause/run state for the node. It is
// sent on connect and whenever the stored state changes; the node applies it
// as its stream gate and reports it in its status.
type DesiredState struct {
	State contract.RunState `json:"state"`
}

// Drain asks the node to disconnect gracefully.
type Drain struct {
	Reason string `json:"reason"`
}

type Upgrade struct {
	Version string `json:"version"`
}
type UpgradeStatus struct {
	Version string `json:"version"`
	State   string `json:"state"`
}

// StreamHeader opens one data stream: exactly one framed JSON message
// followed by raw bytes in both directions.
type StreamHeader struct {
	V          int    `json:"v"`
	TargetHost string `json:"target_host"`
	TargetPort int    `json:"target_port"`
	// SourceIPv6 is the IPv6 source the node must egress from. It is set
	// exactly on a ProtocolVersionSeat header and empty on version 1.
	SourceIPv6 string `json:"source_ipv6,omitempty"`
}

// Source returns the seat source address of a seat-addressed header and
// rejects anything that is not a global IPv6 unicast address.
func (h *StreamHeader) Source() (net.IP, error) {
	ip := net.ParseIP(h.SourceIPv6)
	if ip == nil || ip.To4() != nil {
		return nil, fmt.Errorf("tunnel: stream header source %q is not an IPv6 address", h.SourceIPv6)
	}
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLoopback() {
		return nil, fmt.Errorf("tunnel: stream header source %q is not a public IPv6 address", h.SourceIPv6)
	}
	return ip, nil
}

// Control is the tagged envelope on the control stream: exactly one field is
// set. Any other shape is a protocol error and fails the session.
type Control struct {
	Upgrade       *Upgrade       `json:"upgrade,omitempty"`
	UpgradeStatus *UpgradeStatus `json:"upgrade_status,omitempty"`
	Hello         *Hello         `json:"hello,omitempty"`
	Challenge     *Challenge     `json:"challenge,omitempty"`
	Proof         *Proof         `json:"proof,omitempty"`
	Heartbeat     *Heartbeat     `json:"heartbeat,omitempty"`
	SetState      *SetState      `json:"set_state,omitempty"`
	DesiredState  *DesiredState  `json:"desired_state,omitempty"`
	Drain         *Drain         `json:"drain,omitempty"`
	Reject        *Reject        `json:"reject,omitempty"`
}

// Validate enforces the exactly-one-field envelope invariant.
func (c *Control) Validate() error {
	if c == nil {
		return errors.New("tunnel: nil control message")
	}
	n := 0
	for _, set := range []bool{c.Hello != nil, c.Challenge != nil, c.Proof != nil,
		c.Heartbeat != nil, c.SetState != nil, c.DesiredState != nil, c.Drain != nil, c.Reject != nil, c.Upgrade != nil, c.UpgradeStatus != nil} {
		if set {
			n++
		}
	}
	if n != 1 {
		return fmt.Errorf("tunnel: control message must carry exactly one payload, got %d", n)
	}
	if c.Upgrade != nil && len(c.Upgrade.Version) > 64 {
		return errors.New("tunnel: upgrade version too long")
	}
	if c.UpgradeStatus != nil {
		if len(c.UpgradeStatus.Version) > 64 {
			return errors.New("tunnel: upgrade version too long")
		}
		switch c.UpgradeStatus.State {
		case "started", "failed", "unsupported", "declined":
		default:
			return errors.New("tunnel: invalid upgrade state")
		}
	}
	return nil
}

// ErrMessageTooLarge is returned when a framed length exceeds the cap before
// any allocation happens.
var ErrMessageTooLarge = errors.New("tunnel: control message too large")

// writeFrame writes one length-prefixed JSON message.
func writeFrame(w io.Writer, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("tunnel: encode message: %w", err)
	}
	if len(body) > MaxControlMessageSize {
		return fmt.Errorf("%w: %d bytes", ErrMessageTooLarge, len(body))
	}
	buf := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(buf[:4], uint32(len(body)))
	copy(buf[4:], body)
	if _, err := w.Write(buf); err != nil {
		return fmt.Errorf("tunnel: write message: %w", err)
	}
	return nil
}

// readFrame reads one length-prefixed JSON message into v. A length larger
// than MaxControlMessageSize is rejected BEFORE allocating the body buffer.
func readFrame(r io.Reader, v any) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return fmt.Errorf("tunnel: read frame header: %w", err)
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 {
		return errors.New("tunnel: empty control message")
	}
	if n > MaxControlMessageSize {
		// Rejected before allocation by design; the connection is then closed.
		return fmt.Errorf("%w: %d bytes declared", ErrMessageTooLarge, n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return fmt.Errorf("tunnel: read frame body: %w", err)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("tunnel: decode message: %w", err)
	}
	return nil
}

// WriteControl writes one control envelope.
func WriteControl(w io.Writer, c *Control) error {
	if err := c.Validate(); err != nil {
		return err
	}
	return writeFrame(w, c)
}

// ReadControl reads and validates one control envelope.
func ReadControl(r io.Reader) (*Control, error) {
	var c Control
	if err := readFrame(r, &c); err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate checks a stream header: a known version, the source rule of that
// version, a plausible host and a valid port. Unknown versions are refused (no
// forward compatibility: fail closed).
func (h *StreamHeader) Validate() error {
	if h == nil {
		return errors.New("tunnel: nil stream header")
	}
	switch h.V {
	case ProtocolVersion:
		if h.SourceIPv6 != "" {
			return errors.New("tunnel: version 1 stream header must not carry a source address")
		}
	case ProtocolVersionSeat:
		if _, err := h.Source(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("tunnel: stream header version %d not supported (want %d or %d)", h.V, ProtocolVersion, ProtocolVersionSeat)
	}
	if h.TargetHost == "" || len(h.TargetHost) > 253 {
		return errors.New("tunnel: stream header target host must be 1..253 characters")
	}
	if h.TargetPort < 1 || h.TargetPort > 65535 {
		return errors.New("tunnel: stream header target port out of range")
	}
	return nil
}

// ValidRunState reports whether st is a node-settable run state.
func ValidRunState(st contract.RunState) bool {
	return st == contract.StateRunning || st == contract.StatePaused
}

// itoa is a tiny wrapper so policy.go and session.go share one port encoder.
func itoa(port int) string { return strconv.Itoa(port) }
