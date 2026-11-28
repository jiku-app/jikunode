package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jiku-app/jikunode/nodeagent"
)

func TestPoolBindCLI(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir, _ := configDir()
	id, key, err := nodeagent.EnsureIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	const secret = "fake-Pool-bind-secret"
	secretPath := filepath.Join(t.TempDir(), "pool-secret")
	if err := os.WriteFile(secretPath, []byte(" \t"+secret+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	nonce := bytes.Repeat([]byte{0x91}, 32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/node/bind/challenge":
			var req map[string]any
			if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&req) != nil || req == nil || len(req) != 0 {
				t.Error("challenge must POST an empty JSON object")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"nonce": nonce})
		case "/v1/node/bind":
			var req struct {
				NodeID     string `json:"node_id"`
				PublicKey  []byte `json:"public_key"`
				Name       string `json:"name"`
				Version    string `json:"version"`
				Code       string `json:"code"`
				PoolSecret string `json:"pool_secret"`
				Nonce      []byte `json:"nonce"`
				Signature  []byte `json:"signature"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error("could not decode bind request")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			pub := key.Public().(ed25519.PublicKey)
			if r.Method != http.MethodPost || req.NodeID != id.NodeID || req.Name == "" || req.Version != Version ||
				req.Code != "" || req.PoolSecret != secret || !bytes.Equal(req.Nonce, nonce) || !bytes.Equal(req.PublicKey, pub) {
				t.Error("invalid pool bind payload")
			}
			suffix := id.NodeID + string(nonce) + string(pub) + secret
			if !ed25519.Verify(pub, []byte("jiku-bind-pool-proof-v1:"+suffix), req.Signature) ||
				ed25519.Verify(pub, []byte("jiku-bind-proof-v2:"+suffix), req.Signature) {
				t.Error("invalid pool bind proof or missing domain separation")
			}
			if req.Name == "rejected" {
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "pool_bind_secret_invalid", "detail": secret})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"node_id": id.NodeID})
		default:
			t.Error("unexpected bind endpoint")
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	bind := func(name string, want int) {
		t.Helper()
		var out, errOut bytes.Buffer
		got := run([]string{"bind", "--pool-secret-file", secretPath, "--hub", server.URL, "--name", name}, &out, &errOut)
		if strings.Contains(out.String()+errOut.String(), secret) {
			t.Fatal("pool secret appeared in output")
		}
		if got != want {
			t.Fatalf("bind exited %d, want %d: %s", got, want, errOut.String())
		}
	}
	bind("pool-node", 0)
	cfg, err := nodeagent.LoadConfig(dir)
	if err != nil || !nodeagent.IsBound(dir) {
		t.Fatal("successful bind did not write config and marker", err)
	}
	cfg.PoolPrefixes = []string{"2001:db8:2::/64", "2001:db8:1::/48"}
	path, _ := configPath()
	before, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	bind("rejected", 1)
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, before) {
		t.Fatal("a refused rebind changed the existing config", err)
	}
	bind("renamed-pool-node", 0)
	rebound, err := nodeagent.LoadConfig(dir)
	if err != nil || rebound.Name != "renamed-pool-node" || !slices.Equal(rebound.PoolPrefixes, cfg.PoolPrefixes) {
		t.Fatal("rebind did not update the name and preserve pool prefixes", err)
	}
	after, err = os.ReadFile(path)
	if err != nil || bytes.Contains(after, []byte(secret)) {
		t.Fatal("rebind config could not be read or contained pool secret", err)
	}
}

func TestPoolBindRejectsCodeWithSecretFile(t *testing.T) {
	for i, args := range [][]string{
		{"bind", "ABCDE-FGHIJ", "--pool-secret-file", "missing-file"},
		{"bind", "--pool-secret-file", "missing-file", "ABCDE-FGHIJ"},
		{"bind", "ABCDE-FGHIJ", "--pool-secret-file="},
	} {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			var out, errOut bytes.Buffer
			if got := run(args, &out, &errOut); got != 2 || !strings.Contains(errOut.String(), "cannot combine") {
				t.Fatalf("bind exited %d without the expected usage error: %s", got, errOut.String())
			}
		})
	}
}

func TestPoolBindDoesNotFollowRedirect(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const secret = "fake-redirected-pool-secret"
	secretPath := filepath.Join(t.TempDir(), "pool-secret")
	if err := os.WriteFile(secretPath, []byte(secret), 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/node/bind/challenge":
			_ = json.NewEncoder(w).Encode(map[string]any{"nonce": make([]byte, 32)})
		case "/v1/node/bind":
			http.Redirect(w, r, "/redirect", http.StatusTemporaryRedirect)
		default:
			t.Error("pool bind followed a redirect")
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	var out, errOut bytes.Buffer
	got := run([]string{"bind", "--pool-secret-file", secretPath, "--hub", server.URL}, &out, &errOut)
	if strings.Contains(out.String()+errOut.String(), secret) {
		t.Fatal("pool secret appeared in output")
	}
	if got != 1 || !strings.Contains(errOut.String(), "status 307") {
		t.Fatalf("bind exited %d without reporting the redirect refusal", got)
	}
}
