package fetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/domain"
)

// robotsServer serves a fixed robots.txt and counts every request that is not for it,
// so a test can assert that a disallowed fetch never touched the resource at all.
func robotsServer(t *testing.T, robotsStatus int, robotsBody string) (*httptest.Server, *int64) {
	t.Helper()
	var contentHits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(robotsStatus)
			if robotsStatus < 400 {
				_, _ = w.Write([]byte(robotsBody))
			}
			return
		}
		atomic.AddInt64(&contentHits, 1)
		_, _ = w.Write([]byte("7.25.1"))
	}))
	t.Cleanup(srv.Close)
	return srv, &contentHits
}

func TestRobotsDisallowPreventsTheRequest(t *testing.T) {
	t.Parallel()
	srv, hits := robotsServer(t, http.StatusOK, "User-agent: *\nDisallow: /download/\n")

	f := newTestFetcher(t, srv)
	res, err := f.Fetch(context.Background(), application.FetchRequest{
		URL: srv.URL + "/download/changelogs", RespectRobots: true, Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != domain.OutcomeManualReviewRequired {
		t.Fatalf("outcome = %q, want manual_review_required (err=%v)", res.Outcome, res.Err)
	}
	if n := atomic.LoadInt64(hits); n != 0 {
		t.Fatalf("the disallowed resource was requested %d times; robots.txt was not honoured", n)
	}
	if res.StatusCode != 0 {
		t.Errorf("StatusCode = %d; no request should have been made", res.StatusCode)
	}
}

func TestRobotsAllowCarveOutWins(t *testing.T) {
	t.Parallel()
	srv, hits := robotsServer(t, http.StatusOK,
		"User-agent: *\nDisallow: /download/\nAllow: /download/changelogs\n")

	f := newTestFetcher(t, srv)
	res, err := f.Fetch(context.Background(), application.FetchRequest{
		URL: srv.URL + "/download/changelogs", RespectRobots: true, Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != domain.OutcomeChanged {
		t.Fatalf("outcome = %q, want changed: the longer Allow must win (err=%v)", res.Outcome, res.Err)
	}
	if n := atomic.LoadInt64(hits); n != 1 {
		t.Fatalf("content hits = %d, want 1", n)
	}
}

func TestRobotsMissingMeansNoRestrictions(t *testing.T) {
	t.Parallel()
	srv, hits := robotsServer(t, http.StatusNotFound, "")

	f := newTestFetcher(t, srv)
	res, err := f.Fetch(context.Background(), application.FetchRequest{
		URL: srv.URL + "/changelogs", RespectRobots: true, Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != domain.OutcomeChanged {
		t.Fatalf("outcome = %q, want changed: a 404 robots.txt publishes no restrictions", res.Outcome)
	}
	if n := atomic.LoadInt64(hits); n != 1 {
		t.Fatalf("content hits = %d, want 1", n)
	}
}

func TestRobotsUnknownIsTreatedConservatively(t *testing.T) {
	t.Parallel()
	srv, hits := robotsServer(t, http.StatusInternalServerError, "")

	f := newTestFetcher(t, srv)
	res, err := f.Fetch(context.Background(), application.FetchRequest{
		URL: srv.URL + "/changelogs", RespectRobots: true, Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != domain.OutcomeManualReviewRequired {
		t.Fatalf("outcome = %q, want manual_review_required for an unreadable robots.txt", res.Outcome)
	}
	if n := atomic.LoadInt64(hits); n != 0 {
		t.Fatalf("content hits = %d, want 0", n)
	}

	// The operator override exists and is explicit.
	permissive := newTestFetcher(t, srv, AllowOnUnknownRobots())
	res, err = permissive.Fetch(context.Background(), application.FetchRequest{
		URL: srv.URL + "/changelogs", RespectRobots: true, Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != domain.OutcomeChanged {
		t.Fatalf("with AllowOnUnknownRobots the outcome = %q, want changed", res.Outcome)
	}
}

func TestRobotsIsCachedPerHost(t *testing.T) {
	t.Parallel()
	var robotsHits, contentHits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			atomic.AddInt64(&robotsHits, 1)
			_, _ = w.Write([]byte("User-agent: *\nDisallow: /private/\n"))
			return
		}
		atomic.AddInt64(&contentHits, 1)
	}))
	defer srv.Close()

	f := newTestFetcher(t, srv)
	for i := 0; i < 4; i++ {
		if _, err := f.Fetch(context.Background(), application.FetchRequest{
			URL: srv.URL + "/changelogs", RespectRobots: true, Timeout: time.Second,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if n := atomic.LoadInt64(&robotsHits); n != 1 {
		t.Fatalf("robots.txt was fetched %d times for 4 checks; the cache is not working", n)
	}
	if n := atomic.LoadInt64(&contentHits); n != 4 {
		t.Fatalf("content hits = %d, want 4", n)
	}
}

func TestRobotsRespectsBodyCap(t *testing.T) {
	t.Parallel()
	// A rule that begins after the cap must not be applied, and the fetch must not hang
	// on a robots.txt that never ends.
	big := "User-agent: *\n"
	for i := 0; i < 20000; i++ {
		big += "# padding padding padding padding padding padding\n"
	}
	big += "Disallow: /\n"

	srv, hits := robotsServer(t, http.StatusOK, big)
	f := newTestFetcher(t, srv)
	res, err := f.Fetch(context.Background(), application.FetchRequest{
		URL: srv.URL + "/changelogs", RespectRobots: true, Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != domain.OutcomeChanged {
		t.Fatalf("outcome = %q; a rule past the size cap was applied", res.Outcome)
	}
	if n := atomic.LoadInt64(hits); n != 1 {
		t.Fatalf("content hits = %d, want 1", n)
	}
}

func TestRobotsAllowedAPI(t *testing.T) {
	t.Parallel()

	const body = `
# FirmScout test fixture
User-agent: *
Disallow: /download/
Crawl-delay: 5
Sitemap: https://example.com/sitemap.xml

User-agent: FirmScout
User-agent: SomeOtherBot
Disallow: /private/
Allow: /private/public-changelog
Crawl-delay: 1

User-agent: GreedyBot
Disallow: /
`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			_, _ = w.Write([]byte(body))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	g := NewGuard(WithExemptDestinations(srv.Listener.Addr().String()))
	client := &http.Client{Transport: g.Transport()}
	rc := NewRobotsCache(client, DefaultUserAgent)

	tests := []struct {
		name      string
		agent     string
		path      string
		wantAllow bool
		wantDelay time.Duration
		wantKnown bool
	}{
		{name: "firmscout specific group applies", agent: "FirmScout/0.1 (+https://x)", path: "/private/secret", wantAllow: false, wantDelay: time.Second, wantKnown: true},
		{name: "firmscout allow carve out", agent: "FirmScout/0.1 (+https://x)", path: "/private/public-changelog", wantAllow: true, wantDelay: time.Second, wantKnown: true},
		{name: "firmscout is not bound by the wildcard group", agent: "FirmScout/0.1 (+https://x)", path: "/download/changelogs", wantAllow: true, wantDelay: time.Second, wantKnown: true},
		{name: "unknown agent falls back to the wildcard group", agent: "OtherCrawler/2.0", path: "/download/changelogs", wantAllow: false, wantDelay: 5 * time.Second, wantKnown: true},
		{name: "unknown agent elsewhere is allowed", agent: "OtherCrawler/2.0", path: "/changelogs", wantAllow: true, wantDelay: 5 * time.Second, wantKnown: true},
		{name: "greedy bot group is not ours", agent: "GreedyBot", path: "/anything", wantAllow: false, wantDelay: 0, wantKnown: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			allowed, known, delay, err := rc.Allowed(context.Background(), tc.agent, srv.URL+tc.path)
			if err != nil {
				t.Fatalf("Allowed: %v", err)
			}
			if known != tc.wantKnown {
				t.Fatalf("known = %v, want %v", known, tc.wantKnown)
			}
			if allowed != tc.wantAllow {
				t.Errorf("allowed = %v, want %v", allowed, tc.wantAllow)
			}
			if delay != tc.wantDelay {
				t.Errorf("crawlDelay = %v, want %v", delay, tc.wantDelay)
			}
		})
	}

	sitemaps, known, err := rc.Sitemaps(context.Background(), srv.URL+"/")
	if err != nil || !known {
		t.Fatalf("Sitemaps: known=%v err=%v", known, err)
	}
	if len(sitemaps) != 1 || sitemaps[0] != "https://example.com/sitemap.xml" {
		t.Fatalf("sitemaps = %v", sitemaps)
	}
}

func TestRobotsPathMatch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		pattern string
		path    string
		want    bool
	}{
		{pattern: "/", path: "/anything", want: true},
		{pattern: "/download/", path: "/download/changelogs", want: true},
		{pattern: "/download/", path: "/downloads", want: false},
		{pattern: "/fish", path: "/fishheads", want: true},
		{pattern: "/fish*", path: "/fishheads/x", want: true},
		{pattern: "/*.pdf$", path: "/docs/manual.pdf", want: true},
		{pattern: "/*.pdf$", path: "/docs/manual.pdf?v=2", want: false},
		{pattern: "/private$", path: "/private", want: true},
		{pattern: "/private$", path: "/private/x", want: false},
		{pattern: "/a/*/b", path: "/a/x/y/b", want: true},
		{pattern: "/a/*/b", path: "/a/b", want: false},
		{pattern: "", path: "/", want: false},
	}
	for _, tc := range tests {
		if got := robotsPathMatch(tc.pattern, tc.path); got != tc.want {
			t.Errorf("robotsPathMatch(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}

func TestParseRobotsGroupBoundaries(t *testing.T) {
	t.Parallel()
	// Two agents sharing one group, then a rule, then a new group. Getting this wrong
	// applies another crawler's rules to FirmScout.
	f := parseRobots([]byte("User-agent: a\nUser-agent: b\nDisallow: /x\nUser-agent: c\nDisallow: /y\n"))
	if len(f.groups) != 2 {
		t.Fatalf("got %d groups, want 2: %+v", len(f.groups), f.groups)
	}
	if len(f.groups[0].agents) != 2 {
		t.Fatalf("first group agents = %v, want two", f.groups[0].agents)
	}
	if g := f.selectGroup("c"); g == nil || len(g.rules) != 1 || g.rules[0].pattern != "/y" {
		t.Fatalf("group for c = %+v", g)
	}
	if g := f.selectGroup("a"); g == nil || g.rules[0].pattern != "/x" {
		t.Fatalf("group for a = %+v", g)
	}
}

func TestParseRobotsEmptyDisallowMeansNoRestriction(t *testing.T) {
	t.Parallel()
	f := parseRobots([]byte("User-agent: *\nDisallow:\n"))
	g := f.selectGroup("firmscout")
	if g == nil {
		t.Fatal("no group selected")
	}
	if !g.allows("/anything") {
		t.Fatal("an empty Disallow was treated as a restriction")
	}
}

func TestProductToken(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"FirmScout/0.1 (+https://example)": "firmscout",
		"FirmScout":                        "firmscout",
		"":                                 "",
		"Some Bot/1":                       "some",
	}
	for in, want := range tests {
		if got := productToken(in); got != want {
			t.Errorf("productToken(%q) = %q, want %q", in, got, want)
		}
	}
}
