package jikunode_test

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jiku-app/jikunode/bindcode"
	"github.com/jiku-app/jikunode/contract"
	"github.com/jiku-app/jikunode/nodeagent"
	"github.com/jiku-app/jikunode/tunnel"
)

// These imports and assignments exercise the public boundary used by the hub
// and Android wrapper, without depending on either consumer's implementation.
func TestPublicAPIAndHelloWireShape(t *testing.T) {
	var _ contract.TunnelMetrics = metrics{}
	var _ nodeagent.Hooks = hooks{}
	_ = nodeagent.Runner{Network: nodeagent.Network{Resolver: nil}, Hooks: hooks{}}
	if !errors.Is(contract.ErrNodeDialFailed, contract.ErrNodeDialFailed) {
		t.Fatal("missing shared dial sentinel")
	}
	code, err := bindcode.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bindcode.Normalize(bindcode.Format(code)); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := tunnel.WriteControl(&buf, &tunnel.Control{Hello: &tunnel.Hello{
		NodeID: "node", Version: "test", Nonce: make([]byte, 32), HeartbeatSeconds: 60,
	}}); err != nil {
		t.Fatal(err)
	}
	frame := buf.Bytes()
	if int(binary.BigEndian.Uint32(frame[:4])) != len(frame)-4 {
		t.Fatal("invalid frame length")
	}
	var envelope map[string]map[string]json.RawMessage
	if err := json.Unmarshal(frame[4:], &envelope); err != nil {
		t.Fatal(err)
	}
	hello := envelope["hello"]
	if len(hello) != 4 {
		t.Fatalf("unexpected Hello fields: %v", hello)
	}
	for _, key := range []string{"node_id", "version", "nonce", "heartbeat_seconds"} {
		if _, ok := hello[key]; !ok {
			t.Fatalf("missing %s", key)
		}
	}
}

type metrics struct{}

func (metrics) SessionDelta(int)           {}
func (metrics) Reconnect()                 {}
func (metrics) LeaseExpired()              {}
func (metrics) HeartbeatRTT(time.Duration) {}

type hooks struct{}

func (hooks) OnUnbound()                                       {}
func (hooks) OnBound(*nodeagent.Config)                        {}
func (hooks) OnConnect(*tunnel.ClientSession, <-chan struct{}) {}
func (hooks) OnDisconnect(error)                               {}
func (hooks) OnBackoff(error)                                  {}
func (hooks) Paused() bool                                     { return false }
