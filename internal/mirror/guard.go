// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package mirror

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// Where the upstream may send the registry.
//
// The operator names one upstream address and trusts it: it may well be on
// a private network, or on this machine. What the upstream then answers is
// not trusted the same way. A redirect can name any address, and without a
// check a hub that is compromised, or impersonated on an intercepted
// connection, could make the registry fetch from a service that only the
// registry's own network can reach.
//
// So a request that leaves the configured scheme, host and port:
//
//   - never carries the upstream token (Upstream.get), and
//   - is refused if it leads to a loopback, private, link-local or
//     unspecified address.
//
// The second rule is applied twice. An address written as an IP literal is
// refused before anything is sent. A host name is checked when the
// connection is made, against the address it resolved to, so a name that
// resolves to a private address is caught and a second lookup cannot give a
// different answer than the one that was checked.

// sameOrigin reports whether b is the same scheme, host and port as a.
func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(dialAddr(a), dialAddr(b))
}

// dialAddr is the host:port a connection for u is made to.
func dialAddr(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = "80"
		if strings.EqualFold(u.Scheme, "https") {
			port = "443"
		}
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// isLocal reports whether a is an address that only this machine or its
// own network can reach.
func isLocal(a netip.Addr) bool {
	a = a.Unmap().WithZone("")
	return a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsInterfaceLocalMulticast() || a.IsUnspecified()
}

// guardKey marks a request's context with the host:port that must not turn
// out to be a local address.
type guardKey struct{}

// guardHop prepares a request to target, a host other than the configured
// upstream. It refuses a local address written as a literal, and marks the
// context so the connection is checked when it is made.
func guardHop(ctx context.Context, target *url.URL) (context.Context, error) {
	if ip, err := netip.ParseAddr(target.Hostname()); err == nil && isLocal(ip) {
		return nil, fmt.Errorf("%w: it redirected to %s, a loopback, private or link-local address", ErrUnsafeUpstream, target.Host)
	}
	return context.WithValue(ctx, guardKey{}, dialAddr(target)), nil
}

// guardedClient is the HTTP client used unless Upstream.Client overrides it.
var guardedClient = &http.Client{Transport: guardedTransport()}

// guardedTransport is the default transport with one change: a connection
// made for a guarded hop is refused if it is to a local address.
//
// Only the connection to the hop's own host is checked. When a proxy is
// configured the connection is to the proxy, which is the operator's own
// and resolves the name itself; what it may reach is its policy.
func guardedTransport() http.RoundTripper {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		base = &http.Transport{Proxy: http.ProxyFromEnvironment}
	}
	t := base.Clone()
	open := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	guarded := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second, Control: refuseLocal}
	t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if hop, _ := ctx.Value(guardKey{}).(string); hop != "" && strings.EqualFold(hop, addr) {
			return guarded.DialContext(ctx, network, addr)
		}
		return open.DialContext(ctx, network, addr)
	}
	return t
}

// refuseLocal is a net.Dialer Control hook. It runs after the name has been
// resolved and before the connection is attempted, with the address that
// will actually be connected to.
func refuseLocal(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("%w: cannot tell where %q leads", ErrUnsafeUpstream, address)
	}
	if isLocal(ap.Addr()) {
		return fmt.Errorf("%w: it redirected to a host at %s, a loopback, private or link-local address", ErrUnsafeUpstream, ap.Addr())
	}
	return nil
}
