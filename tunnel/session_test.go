package tunnel

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
)

// A data-stream header must arrive within its deadline or the stream fails.
func TestStreamHeaderDeadline(t *testing.T) {
	c1, c2 := net.Pipe()
	nodeSide, err := yamux.Client(c1, MuxConfig())
	if err != nil {
		t.Fatalf("yamux client: %v", err)
	}
	hubSide, err := yamux.Server(c2, MuxConfig())
	if err != nil {
		t.Fatalf("yamux server: %v", err)
	}
	defer nodeSide.Close()
	defer hubSide.Close()

	// Open a stream but never write the header.
	if _, err := hubSide.OpenStream(); err != nil {
		t.Fatalf("open stream: %v", err)
	}
	stream, err := nodeSide.AcceptStream()
	if err != nil {
		t.Fatalf("accept stream: %v", err)
	}
	defer stream.Close()

	start := time.Now()
	_, err = readStreamHeader(stream, 150*time.Millisecond)
	if err == nil {
		t.Fatal("expected deadline error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("deadline not enforced, took %s", elapsed)
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("expected a timeout error, got %v", err)
	}
}

// A header with the wrong version is refused even when it arrives in time.
func TestStreamHeaderVersion(t *testing.T) {
	c1, c2 := net.Pipe()
	nodeSide, _ := yamux.Client(c1, MuxConfig())
	hubSide, _ := yamux.Server(c2, MuxConfig())
	defer nodeSide.Close()
	defer hubSide.Close()

	// Open a stream by hand and write a v0 header.
	if _, err := hubSide.OpenStream(); err != nil {
		t.Fatalf("open stream: %v", err)
	}
	stream, err := nodeSide.AcceptStream()
	if err != nil {
		t.Fatalf("accept stream: %v", err)
	}
	if err := writeFrame(stream, &StreamHeader{V: 0, TargetHost: "93.184.216.34", TargetPort: 443}); err != nil {
		t.Fatalf("write header: %v", err)
	}
	if _, err := readStreamHeader(stream, time.Second); err == nil {
		t.Fatal("expected version rejection")
	}
}

// NonceLog refuses a repeated nonce (replay defence in depth).
func TestNonceLogRefusesReplay(t *testing.T) {
	var n int
	fixed := func() ([]byte, error) {
		n++
		if n == 1 {
			return make([]byte, NonceSize), nil
		}
		return make([]byte, NonceSize), nil // same value again
	}
	log := NewNonceLogWithSource(fixed)
	if _, err := log.Issue(); err != nil {
		t.Fatalf("first issue: %v", err)
	}
	if _, err := log.Issue(); err == nil {
		t.Fatal("expected replayed nonce to be refused")
	}
}

// The proof binds both nonces and the domain separator.
func TestSignatureMessageBinding(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	helloN := make([]byte, NonceSize)
	chN := make([]byte, NonceSize)
	if _, err := rand.Read(helloN); err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(chN); err != nil {
		t.Fatal(err)
	}
	sig := ed25519.Sign(priv, SignatureMessage(helloN, chN))
	if !ed25519.Verify(pub, SignatureMessage(helloN, chN), sig) {
		t.Fatal("valid proof must verify")
	}
	other := make([]byte, NonceSize)
	if _, err := rand.Read(other); err != nil {
		t.Fatal(err)
	}
	if ed25519.Verify(pub, SignatureMessage(other, chN), sig) {
		t.Fatal("proof must not verify against a different hello nonce")
	}
	if ed25519.Verify(pub, SignatureMessage(helloN, other), sig) {
		t.Fatal("proof must not verify against a different challenge nonce")
	}
	if ed25519.Verify(pub, append(helloN, chN...), sig) {
		t.Fatal("proof must be bound to the domain separator")
	}
	if len(pub) != ed25519.PublicKeySize {
		t.Fatal("sanity")
	}
}
