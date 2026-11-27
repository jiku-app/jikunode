//go:build linux

package tunnel

import "syscall"

// ipv6Freebind is IPV6_FREEBIND (linux/include/uapi/linux/in6.h). It is not
// exported by the syscall package.
const ipv6Freebind = 78

// freebindControl allows a seat-addressed dial to bind a source address that is
// not assigned to an interface. A pool seat is made locally deliverable by a
// local-table route on the pool host, never by an interface address, so bind()
// needs this per-socket opt-in. Only a pool node ever takes this path, and it
// keeps the host from needing net.ipv6.ip_nonlocal_bind.
func freebindControl(network, address string, c syscall.RawConn) error {
	var serr error
	if err := c.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IPV6, ipv6Freebind, 1)
	}); err != nil {
		return err
	}
	return serr
}
