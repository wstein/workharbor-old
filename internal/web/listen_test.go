package web

import (
	"context"
	"errors"
	"net"
	"testing"
)

type addressListener struct {
	net.Listener
	address *net.TCPAddr
	closed  bool
}

func (l *addressListener) Addr() net.Addr { return l.address }
func (l *addressListener) Close() error   { l.closed = true; return nil }

func TestListenBindsOnlyTheConfiguredWebAddress(t *testing.T) {
	for _, address := range []string{"127.0.0.1:8787", "10.44.0.1:8787", "[fd42::1]:8787"} {
		t.Run(address, func(t *testing.T) {
			host, _, _ := net.SplitHostPort(address)
			want := &addressListener{address: &net.TCPAddr{IP: net.ParseIP(host), Port: 8787}}
			got, err := listen(context.Background(), address, func(_ context.Context, network, addr string) (net.Listener, error) {
				if network != "tcp" || addr != address {
					t.Fatalf("bind %s %s", network, addr)
				}
				return want, nil
			})
			if err != nil || got != want {
				t.Fatalf("listen = %v, %v", got, err)
			}
		})
	}
}

func TestListenRefusesWildcardNamesAndUnexpectedBoundAddresses(t *testing.T) {
	for _, address := range []string{"0.0.0.0:8787", "[::]:8787", ":8787", "localhost:8787", "224.0.0.1:8787", "255.255.255.255:8787", "169.254.1.1:8787", "[fe80::1]:8787", "[fe80::1%en0]:8787"} {
		_, err := listen(context.Background(), address, func(context.Context, string, string) (net.Listener, error) {
			t.Fatalf("attempted to bind %s", address)
			return nil, errors.New("unexpected bind")
		})
		if err == nil {
			t.Errorf("accepted %s", address)
		}
	}
	for _, host := range []string{"0.0.0.0", "127.0.0.1"} {
		bound := &addressListener{address: &net.TCPAddr{IP: net.ParseIP(host), Port: 8787}}
		_, err := listen(context.Background(), "10.44.0.1:8787", func(context.Context, string, string) (net.Listener, error) { return bound, nil })
		if err == nil || !bound.closed {
			t.Errorf("unexpected bound address %s: err=%v closed=%v", host, err, bound.closed)
		}
	}
}
