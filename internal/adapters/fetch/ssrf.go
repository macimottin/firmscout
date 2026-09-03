// Package fetch implements FirmScout's guarded HTTP client: the SSRF guard, the
// robots.txt policy cache and the application.Fetcher adapter.
//
// FirmScout is structurally a server-side request forgery machine that has been
// deliberately constrained: it fetches URLs proposed by contributors and discovered by
// AI agents, on a schedule, from inside its own network. The guard in this file is the
// constraint. See docs/architecture/security.md §4.1 (threats T-01, T-02, T-03).
package fetch

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Guard errors. Callers distinguish them with errors.Is; the fetcher maps a blocked
// address onto domain.OutcomeSuspiciousContent so it surfaces for human review rather
// than being retried as a transient network fault.
var (
	// ErrBlockedAddress reports that a destination address is inside a denied range.
	ErrBlockedAddress = errors.New("fetch: destination address is denied by the SSRF guard")
	// ErrBlockedScheme reports a URL scheme other than http or https.
	ErrBlockedScheme = errors.New("fetch: url scheme is not permitted")
	// ErrBlockedPort reports a destination port outside the allow-list.
	ErrBlockedPort = errors.New("fetch: destination port is not permitted")
	// ErrBlockedHost reports a hostname denied by name policy (internal suffixes,
	// userinfo, or a dotless name that would resolve through a search domain).
	ErrBlockedHost = errors.New("fetch: hostname is not permitted")
	// ErrTooManyRedirects reports that the redirect budget was exhausted.
	ErrTooManyRedirects = errors.New("fetch: too many redirects")
)

// deniedRange is one denied CIDR together with why it is denied. The reason is carried
// into the error because "blocked" without "why" produces bug reports rather than
// understanding.
type deniedRange struct {
	net    *net.IPNet
	reason string
}

func mustCIDR(s, reason string) deniedRange {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic("fetch: bad built-in CIDR " + s + ": " + err.Error())
	}
	return deniedRange{net: n, reason: reason}
}

// defaultDeniedRanges is the address policy. It is deliberately a superset of the
// ranges that carry cloud metadata services, because "which address is the metadata
// endpoint" is a per-provider detail that changes, while "link-local is never a
// firmware release page" does not.
//
// Ordering is irrelevant; the whole list is scanned.
func defaultDeniedRanges() []deniedRange {
	return []deniedRange{
		// IPv4.
		mustCIDR("0.0.0.0/8", "this-network (RFC 1122)"),
		mustCIDR("10.0.0.0/8", "private-use (RFC 1918)"),
		mustCIDR("100.64.0.0/10", "carrier-grade NAT (RFC 6598)"),
		mustCIDR("127.0.0.0/8", "loopback (RFC 1122)"),
		mustCIDR("169.254.0.0/16", "link-local, includes cloud instance metadata (RFC 3927)"),
		mustCIDR("172.16.0.0/12", "private-use (RFC 1918)"),
		mustCIDR("192.0.0.0/24", "IETF protocol assignments (RFC 6890)"),
		mustCIDR("192.0.2.0/24", "documentation TEST-NET-1 (RFC 5737)"),
		mustCIDR("192.88.99.0/24", "6to4 relay anycast (RFC 7526)"),
		mustCIDR("192.168.0.0/16", "private-use (RFC 1918)"),
		mustCIDR("198.18.0.0/15", "benchmarking (RFC 2544)"),
		mustCIDR("198.51.100.0/24", "documentation TEST-NET-2 (RFC 5737)"),
		mustCIDR("203.0.113.0/24", "documentation TEST-NET-3 (RFC 5737)"),
		mustCIDR("224.0.0.0/4", "multicast (RFC 5771)"),
		mustCIDR("240.0.0.0/4", "reserved (RFC 1112)"),
		mustCIDR("255.255.255.255/32", "limited broadcast (RFC 8190)"),

		// IPv6.
		mustCIDR("::/128", "unspecified address"),
		mustCIDR("::1/128", "loopback"),
		mustCIDR("100::/64", "discard-only (RFC 6666)"),
		mustCIDR("2001:db8::/32", "documentation (RFC 3849)"),
		mustCIDR("fc00::/7", "unique-local, includes fd00:ec2::254 (RFC 4193)"),
		mustCIDR("fe80::/10", "link-local (RFC 4291)"),
		mustCIDR("ff00::/8", "multicast (RFC 4291)"),
	}
}

// translatedRange is an IPv6 range that embeds an IPv4 address. The embedded address is
// extracted and re-checked against the IPv4 rules rather than the wrapper being treated
// as globally routable, which is how `http://[::ffff:169.254.169.254]/` and
// `http://[64:ff9b::a9fe:a9fe]/` are caught.
type translatedRange struct {
	net    *net.IPNet
	offset int // byte offset of the embedded IPv4 address within the 16-byte form
	label  string
}

var translatedRanges = func() []translatedRange {
	parse := func(s string) *net.IPNet {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			panic("fetch: bad built-in CIDR " + s + ": " + err.Error())
		}
		return n
	}
	// ::ffff:0:0/96 (IPv4-mapped IPv6) is handled directly in unwrapAddress rather than
	// listed here, because net.IPNet.Contains degenerates that prefix to 0.0.0.0/0 --
	// it calls To4 on the network address, and (::ffff:0:0).To4() is 0.0.0.0. A guard
	// that listed it as a denied range would either match every IPv4 address or, more
	// likely, be silently rewritten by a later maintainer who noticed the first effect.
	return []translatedRange{
		{net: parse("64:ff9b::/96"), offset: 12, label: "NAT64 (RFC 6052)"},
		{net: parse("64:ff9b:1::/48"), offset: 12, label: "local-use NAT64 (RFC 8215)"},
		{net: parse("2002::/16"), offset: 2, label: "6to4 (RFC 3056)"},
	}
}()

// deniedHostSuffixes are name-policy denials. They are defence in depth and a better
// error message: the enforcement point is the dialer control hook, because a name check
// is inherently defeated by DNS rebinding.
var deniedHostSuffixes = []string{
	"localhost",
	".localhost",
	".local",
	".localdomain",
	".internal",
	".intranet",
	".lan",
	".corp",
	".private",
	".home.arpa",
	".in-addr.arpa",
	".ip6.arpa",
}

// GuardOption configures a Guard.
type GuardOption func(*Guard)

// Guard decides which destinations FirmScout may connect to.
//
// Known narrowing against docs/architecture/security.md §4.1: that design also calls for
// resolving the hostname up front and rejecting a host if *any* of its A/AAAA records is
// denied ("checked, not raced"). This implementation does not do that pre-flight
// resolution, for two reasons. It costs an extra lookup on every fetch, and it is a
// detection improvement rather than a bypass fix: with the control hook in place, a host
// that advertises one public and one link-local address still cannot be *reached* on the
// link-local one, because that particular connect is refused. What is lost is the signal
// that such a host is suspicious and should be reviewed. If that signal is wanted, the
// place to add it is a resolver hook consulted from ValidateURL, and it must stay defence
// in depth: a pre-flight resolution result is never what the dialer ends up using.
//
// The security property comes from where the check runs, not from what it checks:
// CheckAddr is installed as net.Dialer.Control, so it executes immediately before
// connect(2) with the address the kernel is about to use. A hostname or pre-resolution
// check is a time-of-check/time-of-use hole that DNS rebinding walks straight through;
// a control hook is not, and it collapses every decimal, octal, hexadecimal and
// shorthand IP encoding at once because by then they are all 4- or 16-byte addresses.
type Guard struct {
	denied       []deniedRange
	allowedPorts map[int]bool // nil means every port is allowed
	// exemptDestinations are literal "host:port" strings that bypass the address and
	// port policy. This is the self-hoster escape hatch from
	// docs/architecture/security.md §4.1 in its narrowest form: one approved
	// destination rather than a blanket "allow private networks".
	exemptDestinations map[string]bool
	allowPrivate       bool
	maxRedirects       int
	dialTimeout        time.Duration
	logger             *slog.Logger
}

// WithDeniedCIDRs replaces the denied address ranges wholesale. Passing no arguments
// disables the address policy, which is why AllowPrivateNetworks exists as the explicit,
// warning-emitting way to do that: a policy emptied by accident is the failure mode this
// API is shaped to avoid.
func WithDeniedCIDRs(cidrs ...string) GuardOption {
	return func(g *Guard) {
		ranges := make([]deniedRange, 0, len(cidrs))
		for _, c := range cidrs {
			ranges = append(ranges, mustCIDR(c, "operator-configured"))
		}
		g.denied = ranges
	}
}

// WithAdditionalDeniedCIDRs appends operator-supplied denied ranges, for example a
// self-hoster denying their own corporate supernet.
func WithAdditionalDeniedCIDRs(cidrs ...string) GuardOption {
	return func(g *Guard) {
		for _, c := range cidrs {
			g.denied = append(g.denied, mustCIDR(c, "operator-configured"))
		}
	}
}

// WithAllowedPorts restricts destination ports. The default is 80 and 443: a source
// that needs another port is a registry field a human sets, which turns "reach any
// internal service on any port" into "reach one port a maintainer approved".
func WithAllowedPorts(ports ...int) GuardOption {
	return func(g *Guard) {
		m := make(map[int]bool, len(ports))
		for _, p := range ports {
			m[p] = true
		}
		g.allowedPorts = m
	}
}

// WithoutPortRestriction allows any destination port. Intended for tests, which run
// servers on ephemeral ports.
func WithoutPortRestriction() GuardOption {
	return func(g *Guard) { g.allowedPorts = nil }
}

// WithExemptDestinations allows specific literal "host:port" destinations through the
// address and port policy. Every exemption is logged at construction, because an
// exemption is the one way this guard can be turned into an internal request proxy.
func WithExemptDestinations(addrs ...string) GuardOption {
	return func(g *Guard) {
		if g.exemptDestinations == nil {
			g.exemptDestinations = make(map[string]bool, len(addrs))
		}
		for _, a := range addrs {
			g.exemptDestinations[a] = true
		}
	}
}

// AllowPrivateNetworks disables the address policy entirely. Self-hosters monitoring an
// internal appliance portal legitimately want this, and the moment they enable it their
// FirmScout becomes an internal request proxy, so it warns on every construction.
func AllowPrivateNetworks() GuardOption {
	return func(g *Guard) { g.allowPrivate = true }
}

// WithMaxRedirects caps the redirect chain. The default is 5.
func WithMaxRedirects(n int) GuardOption {
	return func(g *Guard) { g.maxRedirects = n }
}

// WithDialTimeout sets the per-connection dial timeout.
func WithDialTimeout(d time.Duration) GuardOption {
	return func(g *Guard) { g.dialTimeout = d }
}

// WithGuardLogger sets the logger used for start-up warnings.
func WithGuardLogger(l *slog.Logger) GuardOption {
	return func(g *Guard) { g.logger = l }
}

// NewGuard builds a Guard with FirmScout's default policy.
func NewGuard(opts ...GuardOption) *Guard {
	g := &Guard{
		denied:       defaultDeniedRanges(),
		allowedPorts: map[int]bool{80: true, 443: true},
		maxRedirects: 5,
		dialTimeout:  10 * time.Second,
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	for _, o := range opts {
		o(g)
	}
	if g.allowPrivate {
		g.logger.Warn("SSRF guard address policy is disabled: this deployment can reach private and link-local addresses",
			slog.String("control", "ssrf_guard"), slog.Bool("allow_private_networks", true))
	}
	for addr := range g.exemptDestinations {
		g.logger.Warn("SSRF guard exemption in effect",
			slog.String("control", "ssrf_guard"), slog.String("destination", addr))
	}
	return g
}

// MaxRedirects reports the configured redirect budget.
func (g *Guard) MaxRedirects() int { return g.maxRedirects }

// DeniedRanges reports the active denied ranges as CIDR strings with their reasons.
// Exposed so an operator can print exactly what a running deployment enforces.
func (g *Guard) DeniedRanges() []string {
	out := make([]string, 0, len(g.denied))
	for _, r := range g.denied {
		out = append(out, r.net.String()+" ("+r.reason+")")
	}
	return out
}

// CheckAddr is the enforcement point. It is installed as net.Dialer.Control and runs
// immediately before connect(2) with the resolved destination address.
func (g *Guard) CheckAddr(network, address string) error {
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		// Anything that is not a TCP connection is not an HTTP fetch.
		return fmt.Errorf("%w: network %q is not permitted", ErrBlockedAddress, network)
	}
	if g.exemptDestinations[address] {
		return nil
	}
	host, portStr, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%w: %q is not a host:port address: %v", ErrBlockedAddress, address, err)
	}
	// The address handed to Control is always a literal resolved address, never a
	// name. Relying on net.ParseIP here -- rather than on any parsing of the original
	// hostname string -- is what rejects 2130706433, 0177.0.0.1, 0x7f000001 and 127.1
	// without a single special case: they have already collapsed into 127.0.0.1.
	//
	// The address policy is applied before the port policy so that the reported reason
	// is the security-relevant one: "169.254.169.254 is link-local" is a different
	// incident from "port 6379 is not allowed", and conflating them hides the first.
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("%w: %q is not a resolved IP address", ErrBlockedAddress, host)
	}
	if err := g.CheckIP(ip); err != nil {
		return err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return fmt.Errorf("%w: %q is not a numeric port", ErrBlockedPort, portStr)
	}
	if !g.portAllowed(port) {
		return fmt.Errorf("%w: port %d is not in the allow-list", ErrBlockedPort, port)
	}
	return nil
}

// CheckIP applies the address policy to one resolved address, unwrapping IPv4-in-IPv6
// encodings and re-checking the embedded address.
func (g *Guard) CheckIP(ip net.IP) error {
	if g.allowPrivate {
		return nil
	}
	for _, candidate := range unwrapAddress(ip) {
		for _, r := range g.denied {
			if r.net.Contains(candidate.ip) {
				if candidate.via != "" {
					return fmt.Errorf("%w: %s is %s inside %s, which is %s",
						ErrBlockedAddress, ip, candidate.via, r.net, r.reason)
				}
				return fmt.Errorf("%w: %s is inside %s, which is %s",
					ErrBlockedAddress, ip, r.net, r.reason)
			}
		}
	}
	return nil
}

type unwrapped struct {
	ip  net.IP
	via string
}

// unwrapAddress returns the address itself plus every IPv4 address embedded in it.
// Every element is checked, because an attacker only needs one of them to be reachable.
func unwrapAddress(ip net.IP) []unwrapped {
	out := []unwrapped{{ip: ip}}
	if v4 := ip.To4(); v4 != nil {
		// net.IP.To4 already unwraps the ::ffff:0:0/96 form, so an IPv4-mapped IPv6
		// address is checked against the IPv4 rules here and is never treated as a
		// globally routable v6 address.
		if len(ip) == net.IPv6len {
			out = append(out, unwrapped{ip: v4, via: "an IPv4-mapped IPv6 address wrapping " + v4.String()})
		}
		return out
	}
	v6 := ip.To16()
	if v6 == nil {
		return out
	}
	for _, tr := range translatedRanges {
		if !tr.net.Contains(v6) {
			continue
		}
		embedded := net.IPv4(v6[tr.offset], v6[tr.offset+1], v6[tr.offset+2], v6[tr.offset+3]).To4()
		out = append(out, unwrapped{ip: embedded, via: "a " + tr.label + " address wrapping " + embedded.String()})
	}
	return out
}

func (g *Guard) portAllowed(port int) bool {
	if len(g.allowedPorts) == 0 {
		return true
	}
	return g.allowedPorts[port]
}

// ValidateURL applies scheme, userinfo, port and hostname policy before a request is
// made. It is defence in depth and a better error message. It is explicitly *not* the
// control: a hostname that passes here can still resolve to a denied address, which is
// why CheckAddr runs regardless.
func (g *Guard) ValidateURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("%w: %q is not a valid url: %v", ErrBlockedHost, raw, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return nil, fmt.Errorf("%w: %q; only http and https are permitted", ErrBlockedScheme, u.Scheme)
	}
	if u.User != nil {
		// Credentials in a URL are both a leak risk and a classic way to make a
		// hostile host look like a trusted one to a reviewer's eye.
		return nil, fmt.Errorf("%w: url must not carry userinfo", ErrBlockedHost)
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("%w: url has no host", ErrBlockedHost)
	}
	port := u.Port()
	if port == "" {
		if strings.EqualFold(u.Scheme, "https") {
			port = "443"
		} else {
			port = "80"
		}
	}
	if g.exemptDestinations[net.JoinHostPort(host, port)] {
		return u, nil
	}
	if ip := net.ParseIP(host); ip != nil {
		if err := g.CheckIP(ip); err != nil {
			return nil, err
		}
	} else if err := g.checkHostname(host); err != nil {
		return nil, err
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		return nil, fmt.Errorf("%w: %q is not a numeric port", ErrBlockedPort, port)
	}
	if !g.portAllowed(p) {
		return nil, fmt.Errorf("%w: port %d is not in the allow-list", ErrBlockedPort, p)
	}
	return u, nil
}

func (g *Guard) checkHostname(host string) error {
	if g.allowPrivate {
		return nil
	}
	lower := strings.ToLower(strings.TrimSuffix(host, "."))
	if lower == "" {
		return fmt.Errorf("%w: empty hostname", ErrBlockedHost)
	}
	if !strings.Contains(lower, ".") {
		// A dotless name resolves through the resolver's search domains, which on a
		// corporate network is exactly how "http://admin" reaches an internal box.
		return fmt.Errorf("%w: %q has no dot and would resolve through a search domain", ErrBlockedHost, host)
	}
	for _, suffix := range deniedHostSuffixes {
		if lower == strings.TrimPrefix(suffix, ".") || strings.HasSuffix(lower, suffix) {
			return fmt.Errorf("%w: %q matches the denied suffix %q", ErrBlockedHost, host, suffix)
		}
	}
	return nil
}

// Transport builds an *http.Transport whose every connection passes through CheckAddr.
func (g *Guard) Transport() *http.Transport {
	d := &net.Dialer{
		Timeout:   g.dialTimeout,
		KeepAlive: 30 * time.Second,
		Control: func(network, address string, _ syscall.RawConn) error {
			return g.CheckAddr(network, address)
		},
	}
	return &http.Transport{
		DialContext: d.DialContext,
		// Proxy is deliberately nil rather than http.ProxyFromEnvironment. A proxy
		// would make every connection go to the proxy's address, so the control hook
		// would validate the proxy and the real destination would never be checked --
		// an environment variable would silently disable the whole guard.
		Proxy:                 nil,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		// Automatic transparent gzip is disabled so that the compression ratio is
		// observable and the decompressed size can be capped. See fetcher.go.
		DisableCompression: true,
	}
}
