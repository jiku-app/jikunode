package tunnel

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
)

type fakeResolver struct {
	addrs []net.IPAddr
	err   error
}

func (f *fakeResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return f.addrs, f.err
}

// Post-DNS destination policy: every refused class rejects the whole dial,
// including a mixed DNS answer with one public and one private address; the
// dial itself is pinned to the checked IP, never the hostname.
func TestDialPolicy_PrivateIPsAfterDNS(t *testing.T) {
	rejected := []struct {
		name string
		host string
	}{
		{"loopback v4", "127.0.0.1"},
		{"loopback v4 high", "127.255.255.255"},
		{"private 10/8", "10.0.0.1"},
		{"private 172.16/12 low", "172.16.0.1"},
		{"private 172.16/12 high", "172.31.255.254"},
		{"private 192.168/16", "192.168.1.1"},
		{"link-local v4", "169.254.169.254"},
		{"cgnat 100.64/10 low", "100.64.0.1"},
		{"cgnat 100.64/10 high", "100.127.255.255"},
		{"loopback v6", "::1"},
		{"ula fc00::/7", "fc00::1"},
		{"ula fd00::/8", "fd12:3456:789a::1"},
		{"link-local v6", "fe80::1"},
		{"link-local v6 high", "febf::1"},
		{"v4-mapped private", "::ffff:10.0.0.1"},
		{"v4-mapped loopback", "::ffff:127.0.0.1"},
		{"unspecified v4", "0.0.0.0"},
		{"unspecified v6", "::"},
		{"multicast v4", "224.0.0.1"},
		{"multicast v6", "ff02::1"},
		{"broadcast", "255.255.255.255"},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			p := &DialPolicy{}
			if _, err := p.Check(context.Background(), tc.host, 443); err == nil {
				t.Fatalf("expected %s to be refused", tc.host)
			}
		})
	}

	allowed := []string{"93.184.216.34", "8.8.8.8", "100.63.255.255", "100.128.0.1", "172.15.255.255", "172.32.0.1", "2606:2800:220::1"}
	for _, host := range allowed {
		t.Run("public "+host, func(t *testing.T) {
			p := &DialPolicy{}
			ip, err := p.Check(context.Background(), host, 443)
			if err != nil {
				t.Fatalf("expected %s to pass: %v", host, err)
			}
			if ip == nil {
				t.Fatal("nil pinned ip")
			}
		})
	}

	t.Run("hostname resolving public+private is rejected", func(t *testing.T) {
		p := &DialPolicy{Resolver: &fakeResolver{addrs: []net.IPAddr{
			{IP: net.ParseIP("93.184.216.34")},
			{IP: net.ParseIP("10.0.0.5")},
		}}}
		if _, err := p.Check(context.Background(), "vendor.example.com", 443); err == nil {
			t.Fatal("expected mixed-resolution host to be refused")
		}
	})

	t.Run("hostname resolving only public passes and dial is pinned to the IP", func(t *testing.T) {
		var dialed atomic.Value
		p := &DialPolicy{
			Resolver: &fakeResolver{addrs: []net.IPAddr{
				{IP: net.ParseIP("93.184.216.34")},
			}},
			DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				dialed.Store(address)
				return nil, fmt.Errorf("stop here")
			},
		}
		if _, err := p.Dial(context.Background(), "vendor.example.com", 443); err == nil {
			t.Fatal("expected dial error from stub")
		}
		if got, _ := dialed.Load().(string); got != "93.184.216.34:443" {
			t.Fatalf("dial must be pinned to the checked IP, dialed %q", got)
		}
	})

	t.Run("v4-mapped public host is checked after normalization", func(t *testing.T) {
		p := &DialPolicy{Resolver: &fakeResolver{addrs: []net.IPAddr{
			{IP: net.ParseIP("::ffff:93.184.216.34")},
		}}}
		if _, err := p.Check(context.Background(), "mapped.example.com", 443); err != nil {
			t.Fatalf("expected mapped public to pass: %v", err)
		}
	})

	t.Run("port out of range", func(t *testing.T) {
		p := &DialPolicy{}
		for _, port := range []int{0, -1, 65536} {
			if _, err := p.Check(context.Background(), "93.184.216.34", port); err == nil {
				t.Fatalf("expected port %d to be refused", port)
			}
		}
	})
}
