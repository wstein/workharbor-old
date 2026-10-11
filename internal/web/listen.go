package web

import (
	"context"
	"fmt"
	"net"

	"github.com/wstein/workharbor/internal/config"
)

// Listen binds only the web UI's configured IP. It serves no JSON API routes;
// those remain on the host-only unix socket. A non-loopback IP requires the
// operator's VPN and HTTPS setup, and keeps the same web sign-in checks.
func Listen(addr string) (net.Listener, error) {
	var lc net.ListenConfig
	return listen(context.Background(), addr, lc.Listen)
}

func listen(ctx context.Context, addr string, bind func(context.Context, string, string) (net.Listener, error)) (net.Listener, error) {
	ip, err := config.WebListenIP(addr)
	if err != nil {
		return nil, fmt.Errorf("web: %w", err)
	}
	ln, err := bind(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	if tcp, ok := ln.Addr().(*net.TCPAddr); !ok || !tcp.IP.Equal(ip) {
		_ = ln.Close()
		return nil, fmt.Errorf("web: bound %s instead of configured IP %s", ln.Addr(), ip)
	}
	return ln, nil
}
