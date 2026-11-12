package tunnel

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
)

type countedConn struct {
	net.Conn
	enabled atomic.Bool
	bytes   atomic.Int64
}

func (c *countedConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if c.enabled.Load() {
		c.bytes.Add(int64(n))
	}
	return n, err
}

// The wire byte count includes yamux headers: a ping, keepalive or any extra
// control message increases it even if the heartbeat count itself is correct.
func TestIdleHeartbeatWireCadence(t *testing.T) {
	clientWire, serverWire := net.Pipe()
	node, hub := &countedConn{Conn: clientWire}, &countedConn{Conn: serverWire}
	cy, err := yamux.Client(node, MuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer cy.Close()
	sy, err := yamux.Server(hub, MuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer sy.Close()
	ctrl, err := cy.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		s := NewServerSession(sy, nil)
		hello, err := s.AcceptControl(context.Background(), time.Second)
		if err != nil {
			result <- err
			return
		}
		if hello.HeartbeatSeconds != 5 {
			result <- err
			return
		}
		if err = s.SendChallenge(); err != nil {
			result <- err
			return
		}
		if _, err = s.ReadProof(time.Second); err != nil {
			result <- err
			return
		}
		if err = s.SendAck(); err != nil {
			result <- err
			return
		}
		result <- s.ServeControl(ControlHandler{})
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
	if _, err = ReadControl(ctrl); err != nil {
		t.Fatal(err)
	}
	// Allow yamux's stream-open/window handshake to settle before counting idle traffic.
	time.Sleep(100 * time.Millisecond)
	node.enabled.Store(true)
	hub.enabled.Store(true)
	received := make(chan error, 1)
	go func() {
		for {
			ctrl.SetReadDeadline(time.Now().Add(15 * time.Second))
			c, e := ReadControl(ctrl)
			if e != nil {
				received <- e
				return
			}
			if c.Heartbeat == nil {
				received <- err
				return
			}
			if e = WriteControl(ctrl, &Control{Heartbeat: &Heartbeat{}}); e != nil {
				received <- e
				return
			}
		}
	}()
	// Observe three complete 5-second intervals, before the fourth tick.
	time.Sleep(16 * time.Second)
	hub.enabled.Store(false)
	node.enabled.Store(false)
	frame := int64(12 + 4 + len(`{"heartbeat":{}}`))
	if got, want := hub.bytes.Load(), 3*frame; got != want {
		t.Errorf("hub wire bytes = %d, want %d (three heartbeats only)", got, want)
	}
	if got, want := node.bytes.Load(), 3*frame; got != want {
		t.Errorf("node wire bytes = %d, want %d (three echoes only)", got, want)
	}
	select {
	case e := <-result:
		t.Fatalf("hub control loop exited early: %v", e)
	default:
	}
	select {
	case e := <-received:
		t.Fatalf("node control loop exited early: %v", e)
	default:
	}
}
