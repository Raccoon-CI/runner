package local

import (
	"io"
	"net"
	"net/netip"
	"testing"
	"time"
)

func TestOnlyFrom(t *testing.T) {
	serve := func(allow string) string {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ln = OnlyFrom(ln, netip.MustParsePrefix(allow))
		t.Cleanup(func() { ln.Close() })
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				c.Write([]byte("hi"))
				c.Close()
			}
		}()
		return ln.Addr().String()
	}
	read := func(addr string) string {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(5 * time.Second))
		b, _ := io.ReadAll(c)
		return string(b)
	}
	if got := read(serve("127.0.0.0/8")); got != "hi" {
		t.Errorf("peer inside the prefix got %q", got)
	}
	// The listener keeps accepting after turning a peer away.
	other := serve("172.18.0.0/16")
	for range 2 {
		if got := read(other); got != "" {
			t.Errorf("peer outside the prefix got %q", got)
		}
	}
}
