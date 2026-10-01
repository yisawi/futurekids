// Package clientip finds the address of the client behind a trusted reverse proxy.
package clientip

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// Resolver trusts forwarding headers only on connections from one of its proxy prefixes.
type Resolver struct {
	Trusted []netip.Prefix
}

// ParsePrefixes reads a comma-separated list of CIDR prefixes or single addresses.
func ParsePrefixes(list string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, f := range strings.Split(list, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if !strings.Contains(f, "/") {
			a, err := netip.ParseAddr(f)
			if err != nil {
				return nil, fmt.Errorf("invalid proxy address %q", f)
			}
			f = netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()).String()
		}
		p, err := netip.ParsePrefix(f)
		if err != nil {
			return nil, fmt.Errorf("invalid proxy prefix %q", f)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

func (r Resolver) trusted(a netip.Addr) bool {
	for _, p := range r.Trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func parse(s string) (netip.Addr, bool) {
	a, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil {
		return netip.Addr{}, false
	}
	return a.Unmap(), true
}

// Resolve returns the client address. A direct connection is its own client. On a connection
// from a trusted proxy, X-Real-IP is used, then the rightmost X-Forwarded-For entry that is
// not itself a trusted proxy; left-hand X-Forwarded-For entries are client-controlled and
// ignored. identified is false when a trusted proxy did not say who the client is, so the
// address returned is the proxy's and must not be used to tell clients apart.
func (r Resolver) Resolve(req *http.Request) (ip string, identified bool) {
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		host = req.RemoteAddr
	}
	remote, ok := parse(host)
	if !ok {
		return host, false
	}
	if !r.trusted(remote) {
		return remote.String(), true
	}
	if a, ok := parse(req.Header.Get("X-Real-IP")); ok && !r.trusted(a) {
		return a.String(), true
	}
	hops := strings.Split(strings.Join(req.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		a, ok := parse(hops[i])
		if !ok {
			break
		}
		if !r.trusted(a) {
			return a.String(), true
		}
	}
	return remote.String(), false
}
