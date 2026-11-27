package tunnel

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/jiku-app/jikunode/contract"
)

// A seat-addressed header must carry a public IPv6 source; a plain header must
// not carry one at all.
func TestStreamHeaderSeatValidation(t *testing.T) {
	cases := []struct {
		name string
		hdr  StreamHeader
		ok   bool
	}{
		{"v1 plain", StreamHeader{V: ProtocolVersion, TargetHost: "api.anthropic.com", TargetPort: 443}, true},
		{"v1 with source", StreamHeader{V: ProtocolVersion, TargetHost: "a", TargetPort: 443, SourceIPv6: "2600:70ff:a477::2"}, false},
		{"v2 with source", StreamHeader{V: ProtocolVersionSeat, TargetHost: "a", TargetPort: 443, SourceIPv6: "2600:70ff:a477:1::2"}, true},
		{"v2 without source", StreamHeader{V: ProtocolVersionSeat, TargetHost: "a", TargetPort: 443}, false},
		{"v2 ipv4 source", StreamHeader{V: ProtocolVersionSeat, TargetHost: "a", TargetPort: 443, SourceIPv6: "203.0.113.7"}, false},
		{"v2 private source", StreamHeader{V: ProtocolVersionSeat, TargetHost: "a", TargetPort: 443, SourceIPv6: "fd00::1"}, false},
		{"v2 loopback source", StreamHeader{V: ProtocolVersionSeat, TargetHost: "a", TargetPort: 443, SourceIPv6: "::1"}, false},
		{"unknown version", StreamHeader{V: 3, TargetHost: "a", TargetPort: 443}, false},
		{"v1 bad port", StreamHeader{V: ProtocolVersion, TargetHost: "a", TargetPort: 0}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.hdr.Validate()
			if c.ok && err != nil {
				t.Fatalf("expected valid, got %v", err)
			}
			if !c.ok && err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

// A node advertises seat-dial only when it was configured with pool prefixes.
func TestHelloCapabilities(t *testing.T) {
	if (&Hello{}).HasCapability(CapabilitySeatDial) {
		t.Fatal("a plain hello must not claim seat-dial")
	}
	hello := &Hello{Capabilities: []string{CapabilitySeatDial}}
	if !hello.HasCapability(CapabilitySeatDial) {
		t.Fatal("expected seat-dial capability")
	}
	if hello.HasCapability("something-else") {
		t.Fatal("unknown capability must not match")
	}
}

// DialFrom refuses a target with no IPv6 answer instead of falling back to v4.
func TestDialFromNoIPv6(t *testing.T) {
	p := &DialPolicy{Resolver: &fakeResolver{addrs: []net.IPAddr{{IP: net.ParseIP("203.0.113.7")}}}}
	_, err := p.DialFrom(context.Background(), "api.deepseek.com", 443, net.ParseIP("2600:70ff:a477:1::2"))
	if !errors.Is(err, ErrNoIPv6) {
		t.Fatalf("expected ErrNoIPv6, got %v", err)
	}
}

// DialFrom pins the IPv6 answer and dials it with network tcp6 (the source
// itself is applied by the real dialer, which the test seam replaces).
func TestDialFromPinsIPv6(t *testing.T) {
	p := &DialPolicy{Resolver: &fakeResolver{addrs: []net.IPAddr{
		{IP: net.ParseIP("203.0.113.7")},
		{IP: net.ParseIP("2606:4700:4700::1111")},
	}}}
	var gotNetwork, gotAddr string
	p.DialContext = func(_ context.Context, network, address string) (net.Conn, error) {
		gotNetwork, gotAddr = network, address
		c1, c2 := net.Pipe()
		_ = c2.Close()
		return c1, nil
	}
	conn, err := p.DialFrom(context.Background(), "one.one.one.one", 443, net.ParseIP("2600:70ff:a477:1::2"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = conn.Close()
	if gotNetwork != "tcp6" {
		t.Fatalf("network = %q, want tcp6", gotNetwork)
	}
	if gotAddr != "[2606:4700:4700::1111]:443" {
		t.Fatalf("address = %q, want the pinned AAAA", gotAddr)
	}
}

// The public-address rule still refuses the whole answer for a seat dial.
func TestDialFromRefusesPrivateAnswer(t *testing.T) {
	p := &DialPolicy{Resolver: &fakeResolver{addrs: []net.IPAddr{
		{IP: net.ParseIP("2606:4700:4700::1111")},
		{IP: net.ParseIP("fd00::1")},
	}}}
	_, err := p.DialFrom(context.Background(), "vendor.example", 443, net.ParseIP("2600:70ff:a477:1::2"))
	var refused *RefusedAddrError
	if !errors.As(err, &refused) {
		t.Fatalf("expected RefusedAddrError, got %v", err)
	}
}

// seatAllowed is the node-side half of the seat invariant.
func TestSeatAllowed(t *testing.T) {
	prefixes := []netip.Prefix{netip.MustParsePrefix("2600:70ff:a477::/48")}
	s := &ClientSession{cfg: ClientConfig{PoolPrefixes: prefixes}}
	if !s.seatAllowed(net.ParseIP("2600:70ff:a477:abcd::2")) {
		t.Fatal("address inside the pool must be allowed")
	}
	if s.seatAllowed(net.ParseIP("2600:70ff:a478:0::2")) {
		t.Fatal("address outside the pool must be refused")
	}
	if (&ClientSession{cfg: ClientConfig{}}).seatAllowed(net.ParseIP("2600:70ff:a477::2")) {
		t.Fatal("a node without pool prefixes must refuse every seat")
	}
}

// A seat outside the node's pool is refused on the wire with DialSeatRefused.
func TestSeatOutsidePoolRefusedOnWire(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	_ = pub
	sess, err := NewClientSession(ClientConfig{
		NodeID:       "n1",
		PrivateKey:   priv,
		PoolPrefixes: []netip.Prefix{netip.MustParsePrefix("2600:70ff:a477::/48")},
	})
	if err != nil {
		t.Fatalf("client session: %v", err)
	}
	c1, c2 := net.Pipe()
	nodeSide, _ := yamux.Client(c1, MuxConfig())
	hubSide, _ := yamux.Server(c2, MuxConfig())
	defer nodeSide.Close()
	defer hubSide.Close()
	sess.ys = nodeSide

	hs, err := hubSide.OpenStream()
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	stream, err := nodeSide.AcceptStream()
	if err != nil {
		t.Fatalf("accept stream: %v", err)
	}
	if err := writeFrame(hs, &StreamHeader{
		V: ProtocolVersionSeat, TargetHost: "api.anthropic.com", TargetPort: 443,
		SourceIPv6: "2600:70ff:ffff::2",
	}); err != nil {
		t.Fatalf("write header: %v", err)
	}
	go sess.handleStream(context.Background(), stream)

	_ = hs.SetReadDeadline(time.Now().Add(5 * time.Second))
	var resp [2]byte
	if _, err := hs.Read(resp[:]); err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp[0] != dialError || DialCode(resp[1]) != DialSeatRefused {
		t.Fatalf("response = %v, want dialError/%d", resp, byte(DialSeatRefused))
	}
}

// A seat inside the pool with an IPv4-only target fails with DialNoIPv6.
func TestSeatNoIPv6OnWire(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	sess, err := NewClientSession(ClientConfig{
		NodeID:       "n1",
		PrivateKey:   priv,
		Policy:       &DialPolicy{Resolver: &fakeResolver{addrs: []net.IPAddr{{IP: net.ParseIP("203.0.113.7")}}}},
		PoolPrefixes: []netip.Prefix{netip.MustParsePrefix("2600:70ff:a477::/48")},
	})
	if err != nil {
		t.Fatalf("client session: %v", err)
	}
	c1, c2 := net.Pipe()
	nodeSide, _ := yamux.Client(c1, MuxConfig())
	hubSide, _ := yamux.Server(c2, MuxConfig())
	defer nodeSide.Close()
	defer hubSide.Close()
	sess.ys = nodeSide

	hs, err := hubSide.OpenStream()
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	stream, err := nodeSide.AcceptStream()
	if err != nil {
		t.Fatalf("accept stream: %v", err)
	}
	if err := writeFrame(hs, &StreamHeader{
		V: ProtocolVersionSeat, TargetHost: "api.deepseek.com", TargetPort: 443,
		SourceIPv6: "2600:70ff:a477:abcd::2",
	}); err != nil {
		t.Fatalf("write header: %v", err)
	}
	go sess.handleStream(context.Background(), stream)

	_ = hs.SetReadDeadline(time.Now().Add(5 * time.Second))
	var resp [2]byte
	if _, err := hs.Read(resp[:]); err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp[0] != dialError || DialCode(resp[1]) != DialNoIPv6 {
		t.Fatalf("response = %v, want dialError/%d", resp, byte(DialNoIPv6))
	}
}

// A refused dial surfaces as a typed DialRejectedError that still classifies as
// an egress error.
func TestDialRejectedErrorClassification(t *testing.T) {
	err := error(&DialRejectedError{Code: DialNoIPv6})
	if !errors.Is(err, contract.ErrNodeDialFailed) {
		t.Fatal("DialRejectedError must wrap contract.ErrNodeDialFailed")
	}
	var typed *DialRejectedError
	if !errors.As(err, &typed) || typed.Code != DialNoIPv6 {
		t.Fatal("code must survive errors.As")
	}
}
