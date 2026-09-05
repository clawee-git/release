package backend

// The download guard: the bound on every outbound request this service makes.
//
// Ported from the console's internal/console/release/download_guard.go. The
// service fetches from references stored in its own catalog and from GitHub,
// and both are attacker-influenced in the shapes that matter — a catalog row
// carries URLs, and an HTTP redirect is chosen by the far end. Four rules,
// each closing a specific hole:
//
//	https only          a plaintext hop is a rewritable hop
//	no userinfo         https://api.github.com@evil.example is not GitHub
//	no IP literal       an IP literal skips every name-based check below it
//	per-hop, dial the classified address
//	                    checking the URL and then letting the transport resolve
//	                    it again is a TOCTOU: the second lookup can answer with
//	                    a private address. Classify, then dial THAT address.

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"time"
)

// maxRedirects bounds the hop chain. Each hop is checked; a chain longer than
// this is a redirect loop or an attempt to exhaust the checker.
const maxRedirects = 5

// Guard builds bounded HTTP clients.
type Guard struct {
	// AllowPrivate permits private and loopback addresses. Tests set it —
	// httptest listens on 127.0.0.1 — and nothing else does.
	AllowPrivate bool
	Timeout      time.Duration
}

// Client returns an http.Client that enforces the guard on every hop.
func (g *Guard) Client() *http.Client {
	timeout := g.Timeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:                 nil, // a proxy would resolve and dial for us
			DialContext:           g.dial,
			TLSHandshakeTimeout:   30 * time.Second,
			ResponseHeaderTimeout: 5 * time.Minute,
			ExpectContinueTimeout: time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("download guard: more than %d redirects", maxRedirects)
			}
			return g.CheckURL(req.URL)
		},
	}
}

// CheckURL applies the URL-shape rules to one hop.
func (g *Guard) CheckURL(u *url.URL) error {
	if u.Scheme != "https" {
		if !(g.AllowPrivate && u.Scheme == "http") {
			return fmt.Errorf("download guard: %q is not https", u.Redacted())
		}
	}
	if u.User != nil {
		// https://api.github.com@evil.example reads as GitHub to a human and
		// resolves to evil.example.
		return fmt.Errorf("download guard: %q carries userinfo", u.Redacted())
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("download guard: %q has no host", u.Redacted())
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		if !g.AllowPrivate && !isPublic(addr) {
			return fmt.Errorf("download guard: %q addresses a private network", u.Redacted())
		}
		if !g.AllowPrivate {
			return fmt.Errorf("download guard: %q is an IP literal", u.Redacted())
		}
	}
	return nil
}

// dial resolves the host, classifies every answer, and dials the address it
// classified — never the name again.
func (g *Guard) dial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("download guard: %q is not host:port", address)
	}
	addrs, err := (&net.Resolver{}).LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("download guard: resolve %q: %w", host, err)
	}
	var dialer net.Dialer
	dialer.Timeout = 30 * time.Second
	for _, a := range addrs {
		if !g.AllowPrivate && !isPublic(a) {
			// One private answer poisons the name: a round-robin that returns
			// a public address now and a private one on the next lookup is the
			// whole attack.
			return nil, fmt.Errorf("download guard: %q resolves to the private address %s", host, a)
		}
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(a.String(), port))
		if err == nil {
			return conn, nil
		}
	}
	return nil, fmt.Errorf("download guard: could not connect to %q", host)
}

// isPublic reports whether a is a globally routable unicast address.
func isPublic(a netip.Addr) bool {
	a = a.Unmap()
	switch {
	case !a.IsValid(), a.IsLoopback(), a.IsPrivate(), a.IsUnspecified(),
		a.IsLinkLocalUnicast(), a.IsLinkLocalMulticast(), a.IsMulticast(),
		a.IsInterfaceLocalMulticast():
		return false
	}
	// The cloud metadata endpoint is link-local and already excluded, but
	// carrier-grade NAT (100.64/10) is not private by Go's definition and is
	// very much not the public internet.
	if a.Is4() {
		b := a.As4()
		if b[0] == 100 && b[1] >= 64 && b[1] <= 127 {
			return false
		}
	}
	return true
}
