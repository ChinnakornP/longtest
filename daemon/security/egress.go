package security

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ErrEgressDenied is returned for a destination the policy does not allow.
var ErrEgressDenied = errors.New("security: egress denied")

// EgressPolicy decides whether a run may reach a destination.
//
// Deny-by-default, and evaluated in two places for two different reasons:
//
//   - Before the browser navigates. A page controls its own links, redirects,
//     images and fetches, so "the app under test" is not a boundary the
//     browser enforces for us — one <img src> is enough to turn a URL into a
//     GET at an attacker's server, carrying whatever the page put in the query
//     string.
//   - In the egress proxy the sandboxed processes are pointed at, which is
//     where the decision is actually enforced rather than merely made.
//
// A policy is immutable once built; a run gets one and holds it.
type EgressPolicy struct {
	// hosts are exact hostnames.
	hosts map[string]struct{}
	// suffixes are ".example.com" forms matching any subdomain.
	suffixes []string
	// nets are CIDR ranges, for the on-LAN staging case the product exists to
	// serve.
	nets []*net.IPNet
	// allowPrivate permits RFC1918 / loopback / link-local destinations.
	allowPrivate bool
}

// EgressRules is the declarative form a run is configured with.
type EgressRules struct {
	// Allow holds hostnames, ".suffix" patterns and CIDRs.
	Allow []string
	// AllowPrivateNetworks must be set explicitly for a target on the
	// customer's LAN. It is off by default because a policy that silently
	// permits 169.254.169.254 hands a hijacked agent the cloud metadata
	// service, which is the fastest path from "read a web page" to "own the
	// account".
	AllowPrivateNetworks bool
}

// NewEgressPolicy compiles rules. An empty rule set denies everything, which
// is the correct behaviour for a misconfigured run.
func NewEgressPolicy(r EgressRules) (*EgressPolicy, error) {
	p := &EgressPolicy{hosts: map[string]struct{}{}, allowPrivate: r.AllowPrivateNetworks}
	for _, raw := range r.Allow {
		entry := strings.ToLower(strings.TrimSpace(raw))
		switch {
		case entry == "":
			continue
		case entry == "*":
			return nil, errors.New("security: a wildcard egress rule is not an allowlist")
		case strings.Contains(entry, "/"):
			_, n, err := net.ParseCIDR(entry)
			if err != nil {
				return nil, fmt.Errorf("security: egress rule %q: %w", raw, err)
			}
			p.nets = append(p.nets, n)
		case strings.HasPrefix(entry, "."):
			if strings.Count(entry, ".") < 2 {
				// ".com" would allow the internet. A suffix rule has to name
				// at least a registrable-looking domain.
				return nil, fmt.Errorf("security: egress suffix %q is too broad", raw)
			}
			p.suffixes = append(p.suffixes, entry)
		default:
			p.hosts[entry] = struct{}{}
		}
	}
	return p, nil
}

// AllowURL reports whether a run may fetch u.
//
// The scheme check is part of the policy, not a separate concern: `file://`
// turns a navigation into a local file read, and `javascript:` / `data:`
// execute in whatever origin is current. A page that can choose the scheme
// picks one of those.
func (p *EgressPolicy) AllowURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("%w: unparseable url: %w", ErrEgressDenied, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	case "":
		return fmt.Errorf("%w: %q has no scheme", ErrEgressDenied, raw)
	default:
		return fmt.Errorf("%w: scheme %q is not navigable", ErrEgressDenied, u.Scheme)
	}
	if u.User != nil {
		// http://user:pass@host is a credential in a URL, which would then be
		// in the application map, the prompt and the run log.
		return fmt.Errorf("%w: url carries embedded credentials", ErrEgressDenied)
	}
	return p.AllowHost(u.Hostname())
}

// AllowHost reports whether a run may connect to host.
func (p *EgressPolicy) AllowHost(host string) error {
	h := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if h == "" {
		return fmt.Errorf("%w: empty host", ErrEgressDenied)
	}
	// Strip an IPv6 literal's brackets before parsing.
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")

	if ip := net.ParseIP(h); ip != nil {
		return p.allowIP(ip, h)
	}
	if _, ok := p.hosts[h]; ok {
		return nil
	}
	for _, suffix := range p.suffixes {
		if strings.HasSuffix(h, suffix) {
			return nil
		}
	}
	return fmt.Errorf("%w: host %q is not on the allowlist", ErrEgressDenied, host)
}

func (p *EgressPolicy) allowIP(ip net.IP, h string) error {
	if !p.allowPrivate && isSensitiveIP(ip) {
		return fmt.Errorf("%w: %q is a private or link-local address", ErrEgressDenied, h)
	}
	for _, n := range p.nets {
		if n.Contains(ip) {
			return nil
		}
	}
	if _, ok := p.hosts[h]; ok {
		return nil
	}
	return fmt.Errorf("%w: address %q is not on the allowlist", ErrEgressDenied, h)
}

// isSensitiveIP covers the ranges that turn an SSRF into something worse:
// loopback, RFC1918, link-local (169.254.169.254 is the cloud metadata
// service), CGNAT, and the IPv6 equivalents.
func isSensitiveIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsPrivate() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast() {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		// 100.64.0.0/10 carrier-grade NAT, 192.0.0.0/24 IETF protocol
		// assignments, 198.18.0.0/15 benchmarking.
		switch {
		case v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127:
			return true
		case v4[0] == 192 && v4[1] == 0 && v4[2] == 0:
			return true
		case v4[0] == 198 && (v4[1] == 18 || v4[1] == 19):
			return true
		}
	}
	return false
}

// TargetRules builds the rules for a run against a single application.
//
// This is the shape almost every run wants: the app's own origin and nothing
// else. Subdomains are not included — a staging app that serves user content
// from a sibling subdomain is exactly the case where an injected instruction
// gets somewhere useful.
func TargetRules(baseURL string, allowPrivate bool) (EgressRules, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return EgressRules{}, fmt.Errorf("security: target url: %w", err)
	}
	host := u.Hostname()
	if host == "" {
		return EgressRules{}, fmt.Errorf("security: target url %q has no host", baseURL)
	}
	return EgressRules{Allow: []string{host}, AllowPrivateNetworks: allowPrivate}, nil
}
