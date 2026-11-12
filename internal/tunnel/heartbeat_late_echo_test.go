package tunnel

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
)

// A node that slept through several hub heartbeats echoes every queued one on
// wake-up. The hub must accept each echo (consuming the oldest outstanding
// send) and keep the session alive; only silence for 3x the interval fails it.
func TestLateQueuedEchoesKeepSessionAlive(t *testing.T) {
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
	echoes := make(chan time.Duration, 4)
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
		served <- s.ServeControl(ControlHandler{
			OnHeartbeat: func(d time.Duration) { echoes <- d },
		})
	}()
	if err = WriteControl(ctrl, &Control{Hello: &Hello{HeartbeatSeconds: 5}}); err != nil {
		t.Fatal(err)
	}
	if _, err = ReadControl(ctrl); err != nil {
		t.Fatal(err)
	}
	if err = WriteControl(ctrl, &Control{Proof: &Proof{}}); err != nil {
		t.Fatal(err)
	}
	if _, err = ReadControl(ctrl); err != nil { // handshake ack
		t.Fatal(err)
	}
	// Sleep through two hub heartbeats without reading or echoing, then wake
	// and answer both back to back.
	time.Sleep(11 * time.Second)
	for i := 0; i < 2; i++ {
		ctrl.SetReadDeadline(time.Now().Add(2 * time.Second))
		c, err := ReadControl(ctrl)
		if err != nil || c.Heartbeat == nil {
			t.Fatalf("heartbeat %d: %v", i, err)
		}
	}
	for i := 0; i < 2; i++ {
		if err = WriteControl(ctrl, &Control{Heartbeat: &Heartbeat{}}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		select {
		case <-echoes:
		case err := <-served:
			t.Fatalf("session ended on queued echoes: %v", err)
		case <-time.After(2 * time.Second):
			t.Fatalf("echo %d not accepted", i)
		}
	}
	select {
	case err := <-served:
		t.Fatalf("session ended after queued echoes: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
}
