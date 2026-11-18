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

// timeouts bounds how long the registry waits on an upstream host before
// any content flows.
type timeouts struct {
	// dial is the limit on opening a connection.
	dial time.Duration
	// tlsHandshake is the limit on the TLS handshake that follows.
	tlsHandshake time.Duration
	// responseHeader is how long a host may take to start answering once the
	// request has been sent.
	responseHeader time.Duration
}

// upstreamTimeouts is what the default client waits. There is deliberately
// no limit on a whole request: a model file takes as long as it takes, and a
// transfer that is moving is never cut. What is bounded is a host that
// accepts a connection and then says nothing, which would otherwise hold a
// pull-through job until the job's own timeout, hours later.
var upstreamTimeouts = timeouts{
	dial:           30 * time.Second,
	tlsHandshake:   15 * time.Second,
	responseHeader: time.Minute,
}

// keepAlive is how often an idle upstream connection is probed.
const keepAlive = 30 * time.Second

// guardedClient is the HTTP client used unless Upstream.Client overrides it.
// It has no Timeout of its own, for the reason given at upstreamTimeouts.
var guardedClient = &http.Client{Transport: guardedTransport(upstreamTimeouts)}

// guardedTransport is the default transport with two changes: a connection
// made for a guarded hop is refused if it is to a local address, and
// connecting, the TLS handshake and the wait for an answer to begin are
// each limited by limits.
//
// Only the connection to the hop's own host is checked. When a proxy is
// configured the connection is to the proxy, which is the operator's own
// and resolves the name itself; what it may reach is its policy.
func guardedTransport(limits timeouts) *http.Transport {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		base = &http.Transport{Proxy: http.ProxyFromEnvironment}
	}
	t := base.Clone()
	t.TLSHandshakeTimeout = limits.tlsHandshake
	t.ResponseHeaderTimeout = limits.responseHeader
	open := &net.Dialer{Timeout: limits.dial, KeepAlive: keepAlive}
	guarded := &net.Dialer{Timeout: limits.dial, KeepAlive: keepAlive, Control: refuseLocal}
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
