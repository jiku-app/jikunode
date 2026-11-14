package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/jiku-app/jikunode/bindcode"
	"github.com/jiku-app/jikunode/nodeagent"
)

func TestIdentityPersistenceAndConcurrentInitialization(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	ids := make(chan string, 20)
	keys := make(chan string, 20)
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, k, err := nodeagent.EnsureIdentity(dir)
			if err != nil {
				errs <- err
				return
			}
			ids <- c.NodeID
			keys <- string(k)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	close(ids)
	close(keys)
	var id, key string
	for v := range ids {
		if id != "" && id != v {
			t.Fatal("identity changed")
		}
		id = v
	}
	for v := range keys {
		if key != "" && key != v {
			t.Fatal("key changed")
		}
		key = v
	}
	c, k, err := nodeagent.EnsureIdentity(dir)
	if err != nil || c.NodeID != id || string(k) != key {
		t.Fatal("restart changed identity", err)
	}
	if info, err := os.Stat(c.KeyPath); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("key permissions", err)
	}
	if nodeagent.IsBound(dir) {
		t.Fatal("offline identity is already bound")
	}
}

func TestBindProofAndIdentity(t *testing.T) {
	dir := t.TempDir()
	id, key, err := nodeagent.EnsureIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	code, err := bindcode.Generate()
	if err != nil {
		t.Fatal(err)
	}
	nonce := bytes.Repeat([]byte{1}, 32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/node/bind/challenge":
			var req map[string]any
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req) != 0 {
				t.Error("challenge must contain only an empty object")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"nonce": nonce})
		case "/v1/node/bind":
			var req struct {
				NodeID    string `json:"node_id"`
				PublicKey []byte `json:"public_key"`
				Name      string `json:"name"`
				Version   string `json:"version"`
				Code      string `json:"code"`
				Nonce     []byte `json:"nonce"`
				Signature []byte `json:"signature"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
			}
			msg := append([]byte("jiku-bind-proof-v2:"+id.NodeID), nonce...)
			msg = append(msg, key.Public().(ed25519.PublicKey)...)
			msg = append(msg, code...)
			if req.NodeID != id.NodeID || req.Code != code || req.Name != "test-node" || req.Version != "test" || !bytes.Equal(req.Nonce, nonce) || !bytes.Equal(req.PublicKey, key.Public().(ed25519.PublicKey)) || !ed25519.Verify(req.PublicKey, msg, req.Signature) {
				t.Error("invalid bind-proof-v2 payload")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"node_id": id.NodeID})
		default:
			t.Error("binding must not open a tunnel")
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	if _, err = nodeagent.Bind(context.Background(), dir, server.URL, bindcode.Format(code), "test-node", "test", nodeagent.Network{}); err != nil {
		t.Fatal(err)
	}
	id2, key2, err := nodeagent.EnsureIdentity(dir)
	if err != nil || id2.NodeID != id.NodeID || !bytes.Equal(key2, key) {
		t.Fatal("bind changed identity", err)
	}
	if !nodeagent.IsBound(dir) {
		t.Fatal("successful bind did not write marker")
	}
}

func TestInvalidIdentityRefused(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := nodeagent.EnsureIdentity(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "node.id"), []byte("not-a-uuid"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := nodeagent.EnsureIdentity(dir); err == nil {
		t.Fatal("invalid identity accepted")
	}
}
