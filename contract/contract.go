// Package contract contains the tunnel-level contracts shared by JikuNode and
// its hub consumers. Hub-specific storage and authority interfaces belong to
// the hosted service, not the node protocol.
package contract

import (
	"errors"
	"time"
)

type RunState string

const (
	StateRunning RunState = "running"
	StatePaused  RunState = "paused"
)

// ErrNodeDialFailed identifies failures to establish a target connection over
// a node tunnel. Hub consumers must use this same sentinel with errors.Is.
var ErrNodeDialFailed = errors.New("egress: node dial failed")

type TunnelMetrics interface {
	SessionDelta(int)
	Reconnect()
	LeaseExpired()
	HeartbeatRTT(time.Duration)
}
