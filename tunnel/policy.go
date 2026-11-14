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

	var pinned net.IP
	for _, a := range addrs {
		if err := checkIP(a.IP); err != nil {
			return nil, &RefusedAddrError{IP: a.IP}
		}
		if pinned == nil {
			pinned = a.IP
		}
	}
	if pinned == nil {
		return nil, fmt.Errorf("tunnel: %s resolves to no dialable address", host)
	}
	return pinned, nil
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
