package nodeagent

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
	"github.com/jiku-app/jikunode/bindcode"
	"github.com/jiku-app/jikunode/tunnel"
)

// Port of the hub repository's reject-until-rebound regression. Local fakes
// exercise both handshake and live-session rejection without importing the hub.
func TestRunStopsWhenHubRejectsNodeUntilRebound(t *testing.T) {
	for _, tc := range []struct {
		reason string
		live   bool
	}{
		{tunnel.RejectUnknownNode, false}, {tunnel.RejectRevokedNode, true},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			dir := t.TempDir()
			id, key, err := EnsureIdentity(dir)
			if err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			pub := key.Public().(ed25519.PublicKey)
			var rebound atomic.Bool
			var dials atomic.Int32
			connected := make(chan struct{}, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case bindPath + "/challenge":
					_ = json.NewEncoder(w).Encode(map[string]any{"nonce": make([]byte, 32)})
				case bindPath:
					var req struct {
						NodeID    string `json:"node_id"`
						PublicKey []byte `json:"public_key"`
					}
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Error(err)
						return
					}
					if req.NodeID == id.NodeID {
						t.Error("rebind reused rejected identity")
					}
					mu.Lock()
					pub = req.PublicKey
					mu.Unlock()
					rebound.Store(true)
					_ = json.NewEncoder(w).Encode(map[string]string{"node_id": req.NodeID})
				case tunnelPath:
					dials.Add(1)
					ws, err := websocket.Accept(w, r, nil)
					if err != nil {
						return
					}
					defer ws.CloseNow()
					ys, err := yamux.Server(websocket.NetConn(r.Context(), ws, websocket.MessageBinary), tunnel.MuxConfig())
					if err != nil {
						t.Error(err)
						return
					}
					defer ys.Close()
					ss := tunnel.NewServerSession(ys, nil)
					if _, err := ss.AcceptControl(r.Context(), time.Second); err != nil {
						t.Error(err)
						return
					}
					if !rebound.Load() && !tc.live {
						_ = ss.SendReject(tc.reason)
						return
					}
					if err := ss.SendChallenge(); err != nil {
						t.Error(err)
						return
					}
					proof, err := ss.ReadProof(time.Second)
					mu.Lock()
					valid := err == nil && ss.VerifyProof(pub, proof)
					mu.Unlock()
					if !valid {
						t.Error("invalid proof")
						return
					}
					if err := ss.SendAck(); err != nil {
						t.Error(err)
						return
					}
					if !rebound.Load() {
						_ = ss.SendReject(tc.reason)
						return
					}
					connected <- struct{}{}
					_ = ss.ServeControl(tunnel.ControlHandler{})
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			if err := saveConfig(dir, &Config{NodeID: id.NodeID, KeyPath: id.KeyPath, Name: "test", HubURL: server.URL}); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, boundFileName), []byte("bound\n"), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			hooks := &rejectionHooks{unbound: make(chan struct{}, 1), disconnected: make(chan error, 1)}
			runner := &Runner{Dir: dir, Version: "test", Hooks: hooks, PollInterval: time.Millisecond, MinBackoff: time.Millisecond, MaxBackoff: time.Millisecond}
			done := make(chan error, 1)
			go func() { done <- runner.Run(ctx) }()
			defer func() { cancel(); <-done }()
			select {
			case <-hooks.unbound:
			case <-ctx.Done():
				t.Fatal("runner did not idle after rejection")
			}
			select {
			case err := <-hooks.disconnected:
				if !errors.Is(err, tunnel.ErrRejected) {
					t.Fatalf("disconnect: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("missing rejection")
			}
			for _, name := range []string{boundFileName, configFileName, idFileName, keyFileName} {
				if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("rejected identity retained %s: %v", name, err)
				}
			}
			// Wait across several poll/backoff periods: unbound means no dial.
			select {
			case <-time.After(20 * time.Millisecond):
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if dials.Load() != 1 {
				t.Fatalf("redialled while unbound: %d", dials.Load())
			}
			code, err := bindcode.Generate()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Bind(ctx, dir, server.URL, code, "test", "test", Network{}); err != nil {
				t.Fatal(err)
			}
			select {
			case <-connected:
			case <-ctx.Done():
				t.Fatal("did not connect after rebind")
			}
		})
	}
}

type rejectionHooks struct {
	unbound      chan struct{}
	disconnected chan error
}

func (h *rejectionHooks) OnUnbound()                                     { h.unbound <- struct{}{} }
func (*rejectionHooks) OnBound(*Config)                                  {}
func (*rejectionHooks) OnConnect(*tunnel.ClientSession, <-chan struct{}) {}
func (h *rejectionHooks) OnDisconnect(err error) {
	select {
	case h.disconnected <- err:
	default:
	}
}
func (*rejectionHooks) OnBackoff(error) {}
func (*rejectionHooks) Paused() bool    { return false }
