package tunnel

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
)

// A node whose echoes lag by more than maxPendingHeartbeats intervals fails the
// session even when HeartbeatTimeout is far larger, so the pending queue is
// bounded.
func TestHeartbeatPendingQueueIsBounded(t *testing.T) {
	t.Parallel()
	clientWire, serverWire := net.Pipe()
	cy, err := yamux.Client(clientWire, MuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer cy.Close()
	sy, err := yamux.Server(serverWire, MuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer sy.Close()
	ctrl, err := cy.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() {
		s := NewServerSession(sy, nil)
		if _, err := s.AcceptControl(context.Background(), time.Second); err != nil {
			served <- err
			return
		}
		if err := s.SendChallenge(); err != nil {
			served <- err
			return
		}
		if _, err := s.ReadProof(time.Second); err != nil {
			served <- err
			return
		}
		if err := s.SendAck(); err != nil {
			served <- err
			return
		}
		served <- s.ServeControl(ControlHandler{HeartbeatTimeout: time.Hour})
	}()
	if err = WriteControl(ctrl, &Control{Hello: &Hello{HeartbeatSeconds: 1}}); err != nil {
		t.Fatal(err)
	}
	if _, err = ReadControl(ctrl); err != nil {
		t.Fatal(err)
	}
	if err = WriteControl(ctrl, &Control{Proof: &Proof{}}); err != nil {
		t.Fatal(err)
	}
	if _, err = ReadControl(ctrl); err != nil {
		t.Fatal(err)
	}
	// Never echo: the fourth unanswered interval must end the session.
	select {
	case err := <-served:
		if err == nil {
			t.Fatal("session ended without an error")
		}
	case <-time.After((maxPendingHeartbeats + 4) * time.Second):
		t.Fatal("pending heartbeat queue is unbounded: session survived unanswered heartbeats")
	}
}
