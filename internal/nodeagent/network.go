package nodeagent

import (
	"context"
	"errors"
	"net"
	"net/http"
)

// Network applies a resolver to hub HTTP, WebSocket and destination policy traffic.
type Network struct {
	Resolver interface {
		LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
	}
}

// Client returns an HTTP client whose transport dials resolved IPs while retaining
// the original host for TLS SNI and HTTP Host headers.
func (n Network) Client(base *http.Client) *http.Client {
	if base == nil {
		base = http.DefaultClient
	}
	c := *base
	if n.Resolver == nil {
		return &c
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.DialContext = n.DialContext
	c.Transport = tr
	return &c
}

func (n Network) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if n.Resolver == nil {
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	if ip := net.ParseIP(host); ip != nil {
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	ips, err := n.Resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 || ips[0].IP == nil {
		return nil, errors.New("resolver returned no IP")
	}
	return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
}
