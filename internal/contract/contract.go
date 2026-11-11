// Package contract contains the small protocol-facing contracts shared by
// the JikuNode command and tunnel package. Hosted jiku.app implementations
// provide the corresponding hub-side implementations.
package contract

import (
	"context"
	"crypto/ed25519"
	"errors"
	"time"
)

type RunState string

const (
	StateRunning RunState = "running"
	StatePaused  RunState = "paused"
)

var (
	ErrBindCodeInvalid = errors.New("supply: bind code invalid or expired")
	ErrNodeDialFailed  = errors.New("egress: node dial failed")
	ErrNotFound        = errors.New("not found")
)

type EgressNode struct {
	ID       string
	Type     NodeType
	MaxConns int
}

type NodeType string

const NodeAgent NodeType = "agent"

type NodeAuthority interface {
	BindAgent(ctx context.Context, code string, pub ed25519.PublicKey, name, version string) (string, error)
	AgentKey(ctx context.Context, nodeID string) (ed25519.PublicKey, error)
	SetNodeStateFromNode(ctx context.Context, nodeID string, state RunState) error
	TouchNode(ctx context.Context, nodeID, version, ip string) error
}

type TunnelMetrics interface {
	SessionDelta(int)
	Reconnect()
	LeaseExpired()
	HeartbeatRTT(time.Duration)
}
