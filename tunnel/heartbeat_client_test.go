package tunnel

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
)

func TestClientHeartbeatEchoAndTimeout(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serverResult := make(chan error, 1)
	ackAt := make(chan time.Time, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			serverResult <- err
			return
		}
		defer ws.CloseNow()
		ys, err := yamux.Server(websocket.NetConn(r.Context(), ws, websocket.MessageBinary), MuxConfig())
		if err != nil {
			serverResult <- err
			return
		}
		defer ys.Close()
		ss := NewServerSession(ys, nil)
		hello, err := ss.AcceptControl(r.Context(), time.Second*2)
		if err != nil {
			serverResult <- err
			return
		}
		if hello.HeartbeatSeconds != 5 || hello.NodeID != "node-id" {
			serverResult <- errors.New("invalid hello")
			return
		}
		if err = ss.SendChallenge(); err != nil {
			serverResult <- err
			return
		}
		proof, err := ss.ReadProof(2 * time.Second)
		if err != nil || !ss.VerifyProof(key.Public().(ed25519.PublicKey), proof) {
			serverResult <- errors.New("invalid proof")
			return
		}
		if err = ss.SendAck(); err != nil {
			serverResult <- err
			return
		}
		ss.ctrl.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		_, err = ReadControl(ss.ctrl)
		var ne net.Error
		if !errors.As(err, &ne) || !ne.Timeout() {
			serverResult <- errors.New("node echoed ack or sent unsolicited heartbeat")
			return
		}
		ss.ctrl.SetReadDeadline(time.Time{})
		if err = WriteControl(ss.ctrl, &Control{Heartbeat: &Heartbeat{}}); err != nil {
			serverResult <- err
			return
		}
		ss.ctrl.SetReadDeadline(time.Now().Add(time.Second))
		c, err := ReadControl(ss.ctrl)
		if err != nil || c.Heartbeat == nil {
			serverResult <- errors.New("heartbeat echo missing")
			return
		}
		ackAt <- time.Now()
		ss.ctrl.SetReadDeadline(time.Now().Add(18 * time.Second))
		_, err = ReadControl(ss.ctrl)
		if err == nil {
			serverResult <- errors.New("unexpected heartbeat")
			return
		}
		serverResult <- nil
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+server.URL[4:], nil)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := NewClientSession(ClientConfig{NodeID: "node-id", PrivateKey: key, HeartbeatInterval: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- sess.Run(ctx, ws) }()
	sent := <-ackAt
	select {
	case err = <-result:
		if err == nil {
			t.Fatal("expected heartbeat timeout")
		}
		elapsed := time.Since(sent)
		if elapsed < 14*time.Second || elapsed > 17*time.Second {
			t.Fatalf("timeout after %s", elapsed)
		}
	case <-ctx.Done():
		t.Fatal("client did not time out")
	}
	if err = <-serverResult; err != nil {
		t.Fatal(err)
	}
}

func TestHeartbeatIntervalValidation(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	for _, d := range []time.Duration{-time.Second, time.Second, 301 * time.Second, 5500 * time.Millisecond} {
		// Zero alone selects the default; negative inputs must not silently select it.
		if _, err := NewClientSession(ClientConfig{NodeID: "id", PrivateKey: key, HeartbeatInterval: d}); err == nil {
			t.Errorf("accepted %s", d)
		}
	}
	for _, d := range []time.Duration{0, 5 * time.Second, 300 * time.Second} {
		if _, err := NewClientSession(ClientConfig{NodeID: "id", PrivateKey: key, HeartbeatInterval: d}); err != nil {
			t.Errorf("rejected %s: %v", d, err)
		}
	}
	if MuxConfig().EnableKeepAlive {
		t.Fatal("yamux keepalive enabled")
	}
}

func TestServerRejectsInvalidHeartbeatSeconds(t *testing.T) {
	for _, seconds := range []int{0, 4, 301} {
		t.Run(fmt.Sprint(seconds), func(t *testing.T) {
			a, b := net.Pipe()
			client, err := yamux.Client(a, MuxConfig())
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			server, err := yamux.Server(b, MuxConfig())
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			done := make(chan error, 1)
			go func() {
				_, err := NewServerSession(server, nil).AcceptControl(context.Background(), time.Second)
				done <- err
			}()
			stream, err := client.OpenStream()
			if err != nil {
				t.Fatal(err)
			}
			if err = WriteControl(stream, &Control{Hello: &Hello{HeartbeatSeconds: seconds}}); err != nil {
				t.Fatal(err)
			}
			if err = <-done; err == nil {
				t.Fatal("invalid heartbeat accepted")
			}
		})
	}
}
