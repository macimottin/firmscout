package fetch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/macimottin/firmscout/internal/domain"
)

func TestGuardCheckAddr(t *testing.T) {
	t.Parallel()
	g := NewGuard()

	tests := []struct {
		name    string
		address string
		blocked bool
		why     string
	}{
		{name: "ipv4 loopback", address: "127.0.0.1:80", blocked: true, why: "127.0.0.0/8"},
		{name: "ipv4 loopback alternate", address: "127.1.2.3:443", blocked: true, why: "127.0.0.0/8"},
		{name: "cloud metadata", address: "169.254.169.254:80", blocked: true, why: "169.254.0.0/16"},
		{name: "gcp metadata alias address", address: "169.254.169.254:443", blocked: true, why: "169.254.0.0/16"},
		{name: "rfc1918 ten", address: "10.0.0.1:80", blocked: true, why: "10.0.0.0/8"},
		{name: "rfc1918 172", address: "172.16.5.4:80", blocked: true, why: "172.16.0.0/12"},
		{name: "rfc1918 192.168", address: "192.168.1.1:80", blocked: true, why: "192.168.0.0/16"},
		{name: "cgnat", address: "100.64.0.1:80", blocked: true, why: "100.64.0.0/10"},
		{name: "this network", address: "0.0.0.0:80", blocked: true, why: "0.0.0.0/8"},
		{name: "multicast", address: "224.0.0.1:80", blocked: true, why: "224.0.0.0/4"},
		{name: "broadcast", address: "255.255.255.255:80", blocked: true, why: "255.255.255.255/32"},
		{name: "ipv6 loopback", address: "[::1]:80", blocked: true, why: "::1/128"},
		{name: "ipv6 link local", address: "[fe80::1]:80", blocked: true, why: "fe80::/10"},
		{name: "ipv6 unique local", address: "[fd00:ec2::254]:80", blocked: true, why: "fc00::/7"},
		{name: "ipv6 unspecified", address: "[::]:80", blocked: true, why: "::/128"},
		{name: "ipv4 mapped loopback", address: "[::ffff:127.0.0.1]:80", blocked: true, why: "mapped IPv4 loopback"},
		{name: "ipv4 mapped metadata", address: "[::ffff:169.254.169.254]:80", blocked: true, why: "mapped IPv4 link-local"},
		{name: "nat64 metadata", address: "[64:ff9b::a9fe:a9fe]:80", blocked: true, why: "NAT64-wrapped link-local"},
		{name: "6to4 metadata", address: "[2002:a9fe:a9fe::]:80", blocked: true, why: "6to4-wrapped link-local"},
		{name: "documentation range", address: "[2001:db8::1]:80", blocked: true, why: "2001:db8::/32"},

		{name: "public ipv4", address: "93.184.216.34:80", blocked: false},
		{name: "public ipv4 tls", address: "1.1.1.1:443", blocked: false},
		{name: "public ipv6", address: "[2606:4700:4700::1111]:443", blocked: false},
		{name: "public ipv4 mapped", address: "[::ffff:93.184.216.34]:80", blocked: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := g.CheckAddr("tcp", tc.address)
			if tc.blocked {
				if !errors.Is(err, ErrBlockedAddress) {
					t.Fatalf("CheckAddr(%q) = %v, want ErrBlockedAddress (%s)", tc.address, err, tc.why)
				}
				return
			}
			if err != nil {
				t.Fatalf("CheckAddr(%q) = %v, want nil", tc.address, err)
			}
		})
	}
}

func TestGuardCheckAddrRejectsNonTCPAndMalformed(t *testing.T) {
	t.Parallel()
	g := NewGuard()
	for _, addr := range []string{"", "127.0.0.1", "not-an-ip:80", "example.com:80"} {
		if err := g.CheckAddr("tcp", addr); !errors.Is(err, ErrBlockedAddress) && !errors.Is(err, ErrBlockedPort) {
			t.Errorf("CheckAddr(%q) = %v, want a block", addr, err)
		}
	}
	if err := g.CheckAddr("udp", "93.184.216.34:80"); !errors.Is(err, ErrBlockedAddress) {
		t.Errorf("CheckAddr on udp = %v, want ErrBlockedAddress", err)
	}
}

func TestGuardPortPolicy(t *testing.T) {
	t.Parallel()
	g := NewGuard()
	if err := g.CheckAddr("tcp", "93.184.216.34:6379"); !errors.Is(err, ErrBlockedPort) {
		t.Fatalf("CheckAddr on port 6379 = %v, want ErrBlockedPort", err)
	}
	open := NewGuard(WithoutPortRestriction())
	if err := open.CheckAddr("tcp", "93.184.216.34:6379"); err != nil {
		t.Fatalf("port-unrestricted guard blocked 6379: %v", err)
	}
}

func TestGuardValidateURL(t *testing.T) {
	t.Parallel()
	g := NewGuard()

	tests := []struct {
		name    string
		url     string
		wantErr error
	}{
		{name: "https public", url: "https://mikrotik.com/download/changelogs"},
		{name: "http public", url: "http://upgrade.mikrotik.com/routeros/NEWESTa7.stable"},
		{name: "file scheme", url: "file:///etc/passwd", wantErr: ErrBlockedScheme},
		{name: "gopher scheme", url: "gopher://example.com/", wantErr: ErrBlockedScheme},
		{name: "userinfo", url: "http://vendor.example.com@169.254.169.254/", wantErr: ErrBlockedHost},
		{name: "literal metadata", url: "http://169.254.169.254/latest/meta-data/", wantErr: ErrBlockedAddress},
		{name: "literal loopback", url: "http://127.0.0.1/", wantErr: ErrBlockedAddress},
		{name: "literal ipv6 loopback", url: "http://[::1]/", wantErr: ErrBlockedAddress},
		{name: "dotless host", url: "http://admin/", wantErr: ErrBlockedHost},
		{name: "decimal encoded loopback", url: "http://2130706433/", wantErr: ErrBlockedHost},
		{name: "internal suffix", url: "http://metadata.google.internal/", wantErr: ErrBlockedHost},
		{name: "local suffix", url: "http://printer.local/", wantErr: ErrBlockedHost},
		{name: "non standard port", url: "http://example.com:6379/", wantErr: ErrBlockedPort},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := g.ValidateURL(tc.url)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("ValidateURL(%q) = %v, want nil", tc.url, err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ValidateURL(%q) = %v, want %v", tc.url, err, tc.wantErr)
			}
		})
	}
}

// TestControlHookIsTheEnforcementPoint documents why the dialer control hook, and not
// the pre-flight URL check, is the control. "0177.0.0.1" is not an IP literal to Go's
// net.ParseIP, so the hostname policy lets it through -- but a libc resolver parses it
// with inet_aton as 127.0.0.1, and the control hook sees the resolved address.
func TestControlHookIsTheEnforcementPoint(t *testing.T) {
	t.Parallel()
	g := NewGuard()

	if _, err := g.ValidateURL("http://0177.0.0.1/"); err != nil {
		t.Logf("pre-flight already rejected the octal form: %v", err)
	}
	// Whatever the pre-flight decided, the address the kernel would be handed is
	// refused.
	if err := g.CheckAddr("tcp", "127.0.0.1:80"); !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("control hook did not block the resolved address: %v", err)
	}
}

// TestTransportRefusesBlockedAddress proves the guard is wired into a real connection
// attempt: the server is genuinely listening, and the connect is refused before it.
func TestTransportRefusesBlockedAddress(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler was reached; the SSRF guard did not block the connection")
	}))
	defer srv.Close()

	client := &http.Client{Transport: NewGuard(WithoutPortRestriction()).Transport()}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req) //nolint:bodyclose // the request must not succeed
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("request to a loopback server succeeded; the guard is not enforcing")
	}
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("error = %v, want ErrBlockedAddress", err)
	}
}

// TestRedirectToBlockedAddressIsRefused covers T-03: the initial host is permitted and
// benign, and it answers with a redirect into the denied range.
func TestRedirectToBlockedAddressIsRefused(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1:1/", http.StatusFound)
	}))
	defer srv.Close()

	f := newTestFetcher(t, srv)
	res, err := f.Fetch(context.Background(), fetchRequest(srv.URL))
	if err != nil {
		t.Fatalf("Fetch returned an error: %v", err)
	}
	if res.Outcome != domain.OutcomeSuspiciousContent {
		t.Fatalf("outcome = %q, want %q (err=%v)", res.Outcome, domain.OutcomeSuspiciousContent, res.Err)
	}
	if !errors.Is(res.Err, ErrBlockedAddress) {
		t.Fatalf("res.Err = %v, want ErrBlockedAddress", res.Err)
	}
	if len(res.Body) != 0 {
		t.Fatalf("body was populated from a refused redirect: %q", res.Body)
	}
}

func TestRedirectBudgetIsBounded(t *testing.T) {
	t.Parallel()
	var hops int
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		hops++
		http.Redirect(w, r, "/again", http.StatusFound)
	})
	mux.HandleFunc("/again", func(w http.ResponseWriter, r *http.Request) {
		hops++
		http.Redirect(w, r, "/", http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	f := newTestFetcher(t, srv)
	res, _ := f.Fetch(context.Background(), fetchRequest(srv.URL+"/"))
	if res.Outcome != domain.OutcomeUnavailable {
		t.Fatalf("outcome = %q, want %q", res.Outcome, domain.OutcomeUnavailable)
	}
	if !errors.Is(res.Err, ErrTooManyRedirects) {
		t.Fatalf("res.Err = %v, want ErrTooManyRedirects", res.Err)
	}
	if hops > 7 {
		t.Fatalf("server saw %d hops; the redirect budget is not bounded", hops)
	}
}

func TestGuardExemptionAndPrivateOverride(t *testing.T) {
	t.Parallel()
	exempt := NewGuard(WithExemptDestinations("127.0.0.1:9999"))
	if err := exempt.CheckAddr("tcp", "127.0.0.1:9999"); err != nil {
		t.Fatalf("exempt destination was blocked: %v", err)
	}
	if err := exempt.CheckAddr("tcp", "127.0.0.1:9998"); !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("a neighbouring port was allowed by the exemption: %v", err)
	}

	open := NewGuard(AllowPrivateNetworks(), WithoutPortRestriction())
	if err := open.CheckAddr("tcp", "10.1.2.3:8080"); err != nil {
		t.Fatalf("AllowPrivateNetworks did not permit a private address: %v", err)
	}
}

func TestDeniedRangesAreReportable(t *testing.T) {
	t.Parallel()
	ranges := NewGuard().DeniedRanges()
	if len(ranges) < 20 {
		t.Fatalf("only %d denied ranges: %v", len(ranges), ranges)
	}
	joined := strings.Join(ranges, " ")
	for _, must := range []string{"169.254.0.0/16", "127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "fc00::/7", "fe80::/10", "::1/128"} {
		if !strings.Contains(joined, must) {
			t.Errorf("denied ranges do not include %s", must)
		}
	}
}
