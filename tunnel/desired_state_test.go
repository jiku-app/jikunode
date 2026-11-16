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

	"github.com/jiku-app/jikunode/contract"
)

// The node takes its run state from the hub only: the hub sends it right after
// the handshake ack, and the client hands it to OnDesiredState.
func TestClientAppliesHubDesiredState(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	serverDone := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverDone <- func() error {
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
			if err = ss.SendDesiredState(contract.StatePaused); err != nil {
				return err
			}
			<-release
			return nil
		}()
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+server.URL[4:], nil)
	if err != nil {
		t.Fatal(err)
	}
	states := make(chan contract.RunState, 1)
	sess, err := NewClientSession(ClientConfig{NodeID: "node", PrivateKey: key, OnDesiredState: func(st contract.RunState) {
		states <- st
	}})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = sess.Run(ctx, ws) }()

	select {
	case st := <-states:
		if st != contract.StatePaused {
			t.Fatalf("desired state = %q", st)
		}
	case <-ctx.Done():
		t.Fatal("hub-pushed state never reached the node")
	}
	close(release)
	cancel()
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}
