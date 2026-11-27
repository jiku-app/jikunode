//go:build !linux

package tunnel

import (
	"errors"
	"syscall"
)

// freebindControl fails closed off Linux: a seat-addressed dial is a pool-node
// feature and every pool node is a Linux host, so there is no fallback path
// that would silently egress from the wrong address.
func freebindControl(network, address string, c syscall.RawConn) error {
	return errors.New("tunnel: seat dial requires a Linux host (IPV6_FREEBIND)")
}
