package tunnel

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
)

func TestUpgradeKeepsHeartbeatAndExitsOnSuccess(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	complete := make(chan struct{})
	serverDone := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		run := func() error {
			ws, err := websocket.Accept(w, r, nil)
			if err != nil {
				return err
			}
			defer ws.CloseNow()
			ys, err := yamux.Server(websocket.NetConn(r.Context(), ws, websocket.MessageBinary), MuxConfig())
			if err != nil {
				return err
			}
			defer ys.Close()
			ss := NewServerSession(ys, nil)
			if _, err = ss.AcceptControl(r.Context(), time.Second); err != nil {
				return err
			}
			if err = ss.SendChallenge(); err != nil {
				return err
			}
			proof, err := ss.ReadProof(time.Second)
			if err != nil {
				return err
			}
			if !ss.VerifyProof(pub, proof) {
				return errors.New("bad proof")
			}
			if err = ss.SendAck(); err != nil {
				return err
			}
			if err = ss.SendUpgrade("v2.0.0"); err != nil {
				return err
			}
			_ = ss.ctrl.SetReadDeadline(time.Now().Add(time.Second))
			c, err := ReadControl(ss.ctrl)
			if err != nil {
				return err
			}
			if c.UpgradeStatus == nil || c.UpgradeStatus.State != "started" {
				return errors.New("missing started")
			}
			// Keep the upgrade blocked while a real heartbeat is exchanged.
			if err = WriteControl(ss.ctrl, &Control{Heartbeat: &Heartbeat{}}); err != nil {
				return err
			}
			c, err = ReadControl(ss.ctrl)
			if err != nil {
				return err
			}
			if c.Heartbeat == nil {
				return errors.New("heartbeat blocked by upgrade")
			}
			close(complete)
			// Keep the server stream open until the successful client exits.
			_, err = ReadControl(ss.ctrl)
			if err == nil {
				return errors.New("unexpected control")
			}
			return nil
		}
		serverDone <- run()
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+server.URL[4:], nil)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := NewClientSession(ClientConfig{NodeID: "node", Version: "v1.0.0", PrivateKey: key, OnUpgrade: func(ctx context.Context, v string, status func(string) error) error {
		if err := status("started"); err != nil {
			return err
		}
		select {
		case <-complete:
			return ErrUpgraded
		case <-ctx.Done():
			return ctx.Err()
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err = sess.Run(ctx, ws); !errors.Is(err, ErrUpgraded) {
		t.Fatalf("success exit: %v", err)
	}
	if err = <-serverDone; err != nil {
		t.Fatal(err)
	}
}
