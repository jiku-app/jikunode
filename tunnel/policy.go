package tunnel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"
)

// Resolver is the DNS seam of the destination policy. Production uses
// net.DefaultResolver; tests inject deterministic answers.
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// DialPolicy is the node-side destination policy: after DNS resolution the
// node refuses every non-public address (private, loopback, link-local,
// CGNAT, IPv6 ULA, unspecified, multicast and the IPv4 broadcast address) if
// ANY resolved address falls into a refused class, and then dials the checked
// IP directly — never the hostname — so the DNS answer cannot change between
// check and dial. There is deliberately no host or port allowlist.
type DialPolicy struct {
	// Resolver resolves the target host. Nil means net.DefaultResolver.
	Resolver Resolver

	// Timeout bounds the direct dial of the checked IP. Zero means 10s.
	Timeout time.Duration

	// DialContext dials the pinned address. It exists for tests; production
	// leaves it nil and a net.Dialer with Timeout is used.
	DialContext func(ctx context.Context, network, address string) (net.Conn, error)
}

// DefaultDialPolicy returns the production policy.
func DefaultDialPolicy() *DialPolicy { return &DialPolicy{} }

func (p *DialPolicy) resolver() Resolver {
	if p == nil || p.Resolver == nil {
		return net.DefaultResolver
	}
	return p.Resolver
}

func (p *DialPolicy) timeout() time.Duration {
	if p == nil || p.Timeout <= 0 {
		return 10 * time.Second
	}
	return p.Timeout
}

// cgnatRange is 100.64.0.0/10 (RFC 6598 carrier-grade NAT).
var cgnatRange = func() *net.IPNet {
	_, n, err := net.ParseCIDR("100.64.0.0/10")
	if err != nil {
		panic(err)
	}
	return n
}()

// ErrNoIPv6 reports that a seat-addressed dial found no IPv6 address for the
// target. It is distinct from a transport failure so the hub can fail over
// deliberately instead of treating it as a node fault.
var ErrNoIPv6 = errors.New("tunnel: target has no IPv6 address")

// RefusedAddrError reports that a resolved address belongs to a refused
// class; the address itself is included because it is a dial target, never a
// secret.
type RefusedAddrError struct{ IP net.IP }

func (e *RefusedAddrError) Error() string {
	return fmt.Sprintf("tunnel: refused non-public address %s", e.IP)
}

// checkIP normalizes IPv4-mapped IPv6 addresses and reports whether the
// address belongs to a refused class.
func checkIP(ip net.IP) error {
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	switch {
	case ip.IsUnspecified():
		return fmt.Errorf("tunnel: refused unspecified address %s", ip)
	case ip.IsLoopback():
		return fmt.Errorf("tunnel: refused loopback address %s", ip)
	case ip.IsMulticast():
		return fmt.Errorf("tunnel: refused multicast address %s", ip)
	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast():
		// 169.254/16 and fe80::/10
		return fmt.Errorf("tunnel: refused link-local address %s", ip)
	case ip.IsPrivate():
		// 10/8, 172.16/12, 192.168/16 and IPv6 ULA fc00::/7
		return fmt.Errorf("tunnel: refused private address %s", ip)
	case cgnatRange.Contains(ip):
		return fmt.Errorf("tunnel: refused CGNAT address %s", ip)
	case ip.Equal(net.IPv4bcast):
		// 255.255.255.255 is never a dialable public target.
		return fmt.Errorf("tunnel: refused broadcast address %s", ip)
	}
	return nil
}

// Check resolves host and returns the single public IP to dial. If ANY
// resolved address is private, loopback, link-local, CGNAT, ULA, unspecified
// or multicast, the whole dial is refused (fail closed: one bad address in a
// round-robin answer must not leak the connection).
func (p *DialPolicy) Check(ctx context.Context, host string, port int) (net.IP, error) {
	ips, err := p.resolveChecked(ctx, host, port)
	if err != nil {
		return nil, err
	}
	return ips[0], nil
}

// resolveChecked resolves host and applies the public-address rule to every
// answer, returning them in resolver order. It is the single place the refusal
// policy is implemented, so Check and DialFrom cannot drift apart.
func (p *DialPolicy) resolveChecked(ctx context.Context, host string, port int) ([]net.IP, error) {
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("tunnel: port %d out of range", port)
	}
	if host == "" || len(host) > 253 {
		return nil, errors.New("tunnel: target host must be 1..253 characters")
	}

	var addrs []net.IPAddr
	if ip := net.ParseIP(host); ip != nil {
		addrs = []net.IPAddr{{IP: ip}}
	} else {
		var err error
		addrs, err = p.resolver().LookupIPAddr(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("tunnel: resolve %s: %w", host, err)
		}
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("tunnel: %s resolves to no address", host)
	}

	ips := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		if err := checkIP(a.IP); err != nil {
			return nil, &RefusedAddrError{IP: a.IP}
		}
		ips = append(ips, a.IP)
	}
	return ips, nil
}

// Dial checks the destination and then dials the checked IP directly (the
// hostname is never re-resolved by the dialer).
func (p *DialPolicy) Dial(ctx context.Context, host string, port int) (net.Conn, error) {
	ip, err := p.Check(ctx, host, port)
	if err != nil {
		return nil, err
	}
	addr := net.JoinHostPort(ip.String(), itoa(port))
	if p != nil && p.DialContext != nil {
		return p.DialContext(ctx, "tcp", addr)
	}
	var d net.Dialer
	d.Timeout = p.timeout()
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("tunnel: dial %s: %w", ip, err)
	}
	return conn, nil
}

// DialFrom is Dial with a pinned IPv6 source address: a Jiku egress-pool seat.
// It keeps the same post-DNS refusal rule, dials only an AAAA answer (IPv6 is
// the whole point of a seat, so an IPv4-only target is ErrNoIPv6 and never a
// silent fallback to another address), and binds source as the local address.
func (p *DialPolicy) DialFrom(ctx context.Context, host string, port int, source net.IP) (net.Conn, error) {
	if source == nil || source.To4() != nil || source.IsUnspecified() {
		return nil, errors.New("tunnel: seat source must be an IPv6 address")
	}
	ips, err := p.resolveChecked(ctx, host, port)
	if err != nil {
		return nil, err
	}
	var pinned net.IP
	for _, ip := range ips {
		if ip.To4() == nil {
			pinned = ip
			break
		}
	}
	if pinned == nil {
		return nil, fmt.Errorf("%w: %s", ErrNoIPv6, host)
	}
	addr := net.JoinHostPort(pinned.String(), itoa(port))
	if p != nil && p.DialContext != nil {
		return p.DialContext(ctx, "tcp6", addr)
	}
	// Control lets the socket bind a source that no interface carries: the pool
	// host only makes the seat prefix locally deliverable (local-table route),
	// so bind() needs IPV6_FREEBIND. Fail closed off Linux.
	d := net.Dialer{Timeout: p.timeout(), LocalAddr: &net.TCPAddr{IP: source}, Control: freebindControl}
	conn, err := d.DialContext(ctx, "tcp6", addr)
	if err != nil {
		return nil, fmt.Errorf("tunnel: dial %s from %s: %w", pinned, source, err)
	}
	return conn, nil
}
