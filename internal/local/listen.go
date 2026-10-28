package local

import (
	"net"
	"net/netip"
)

// OnlyFrom wraps ln so that it hands out only connections whose peer is in
// allow; the others are closed as they are accepted. It is for listeners on a
// docker network's gateway: that address is the host's own, so containers on
// other networks could reach it too.
func OnlyFrom(ln net.Listener, allow netip.Prefix) net.Listener {
	return &prefixListener{Listener: ln, allow: allow}
}

type prefixListener struct {
	net.Listener
	allow netip.Prefix
}

func (l *prefixListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if ap, err := netip.ParseAddrPort(c.RemoteAddr().String()); err == nil && l.allow.Contains(ap.Addr().Unmap()) {
			return c, nil
		}
		c.Close()
	}
}
