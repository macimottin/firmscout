package fetch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Robots policy defaults.
const (
	// DefaultRobotsTTL is how long a successfully retrieved robots.txt is trusted.
	DefaultRobotsTTL = 24 * time.Hour
	// DefaultRobotsNegativeTTL is how long an UNKNOWN result is remembered. It is much
	// shorter than the success TTL because an unreachable robots.txt is usually a
	// transient fault, and remembering it for a day would stop collection from a host
	// for a day on the strength of one 503.
	DefaultRobotsNegativeTTL = 10 * time.Minute
	// DefaultRobotsMaxBytes caps the robots.txt body. Google's limit is 500 KiB; this
	// is 512 KiB, and content past the cap is discarded rather than truncating a rule
	// mid-line.
	DefaultRobotsMaxBytes = 512 * 1024
)

// robotsRule is one Allow or Disallow directive.
type robotsRule struct {
	allow   bool
	pattern string
}

// robotsGroup is one user-agent group.
type robotsGroup struct {
	agents        []string // lower-cased product tokens; "*" is the wildcard group
	rules         []robotsRule
	crawlDelay    time.Duration
	hasCrawlDelay bool
}

// robotsFile is a parsed robots.txt.
type robotsFile struct {
	groups   []robotsGroup
	sitemaps []string
}

// robotsEntry is one cached host policy.
type robotsEntry struct {
	mu        sync.Mutex // serialises fetches for this host
	file      *robotsFile
	known     bool
	fetchedAt time.Time
	err       error
}

// RobotsCache fetches, caches and evaluates robots.txt per host.
//
// FirmScout never evades robots.txt (docs/architecture/update-pipeline.md, "the
// absolute prohibitions"). This type exists to obey it, not to find a way around it.
type RobotsCache struct {
	client      *http.Client
	userAgent   string
	ttl         time.Duration
	negativeTTL time.Duration
	maxBytes    int64
	now         func() time.Time

	mu      sync.Mutex
	entries map[string]*robotsEntry
}

// RobotsOption configures a RobotsCache.
type RobotsOption func(*RobotsCache)

// WithRobotsTTL sets how long a successful robots.txt is trusted.
func WithRobotsTTL(d time.Duration) RobotsOption {
	return func(r *RobotsCache) { r.ttl = d }
}

// WithRobotsNegativeTTL sets how long an UNKNOWN result is remembered.
func WithRobotsNegativeTTL(d time.Duration) RobotsOption {
	return func(r *RobotsCache) { r.negativeTTL = d }
}

// WithRobotsMaxBytes caps the robots.txt body size.
func WithRobotsMaxBytes(n int64) RobotsOption {
	return func(r *RobotsCache) { r.maxBytes = n }
}

// WithRobotsClock injects a clock, which is what makes TTL expiry testable.
func WithRobotsClock(now func() time.Time) RobotsOption {
	return func(r *RobotsCache) { r.now = now }
}

// NewRobotsCache builds a cache that fetches through the supplied client. The client
// must be the guarded one: robots.txt is fetched from the same untrusted hosts as
// everything else.
func NewRobotsCache(client *http.Client, userAgent string, opts ...RobotsOption) *RobotsCache {
	r := &RobotsCache{
		client:      client,
		userAgent:   userAgent,
		ttl:         DefaultRobotsTTL,
		negativeTTL: DefaultRobotsNegativeTTL,
		maxBytes:    DefaultRobotsMaxBytes,
		now:         time.Now,
		entries:     make(map[string]*robotsEntry),
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Allowed reports whether userAgent may fetch rawURL.
//
// known reports whether the policy is actually known. A 404 (or any 4xx) means the host
// publishes no restrictions, which is known and permissive. A 5xx, a timeout or a
// transport failure means UNKNOWN: the caller decides, and FirmScout's fetcher refuses
// by default, because "we could not read the rules" is not "the rules permit this".
func (r *RobotsCache) Allowed(ctx context.Context, userAgent, rawURL string) (allowed bool, known bool, crawlDelay time.Duration, err error) {
	u, perr := url.Parse(rawURL)
	if perr != nil {
		return false, false, 0, perr
	}
	file, known, ferr := r.policyFor(ctx, u)
	if !known {
		return false, false, 0, ferr
	}
	group := file.selectGroup(productToken(userAgent))
	if group == nil {
		return true, true, 0, nil
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	return group.allows(path), true, group.crawlDelay, nil
}

// Sitemaps returns the sitemap URLs advertised by the host's robots.txt. The sitemap
// watcher mechanism in the ladder is cheaper than any page hash, and this is where the
// sitemap is discovered.
func (r *RobotsCache) Sitemaps(ctx context.Context, rawURL string) ([]string, bool, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, false, err
	}
	file, known, ferr := r.policyFor(ctx, u)
	if !known {
		return nil, false, ferr
	}
	out := make([]string, len(file.sitemaps))
	copy(out, file.sitemaps)
	return out, true, nil
}

func (r *RobotsCache) policyFor(ctx context.Context, u *url.URL) (*robotsFile, bool, error) {
	key := strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)

	r.mu.Lock()
	entry, ok := r.entries[key]
	if !ok {
		entry = &robotsEntry{}
		r.entries[key] = entry
	}
	r.mu.Unlock()

	entry.mu.Lock()
	defer entry.mu.Unlock()

	if !entry.fetchedAt.IsZero() {
		ttl := r.ttl
		if !entry.known {
			ttl = r.negativeTTL
		}
		if r.now().Sub(entry.fetchedAt) < ttl {
			return entry.file, entry.known, entry.err
		}
	}

	file, known, err := r.fetch(ctx, u.Scheme+"://"+u.Host+"/robots.txt")
	entry.file, entry.known, entry.err, entry.fetchedAt = file, known, err, r.now()
	return file, known, err
}

func (r *RobotsCache) fetch(ctx context.Context, robotsURL string) (*robotsFile, bool, error) {
	u, err := url.Parse(robotsURL)
	if err != nil {
		return nil, false, err
	}
	// A robots.txt that redirects to a CDN on another host is normal and is followed;
	// the compliance boundary that a redirect must not cross applies to source content,
	// not to policy discovery. The address policy and the redirect budget still apply.
	st := &redirectState{originalHost: u.Hostname(), allowOffHost: true}
	req, err := http.NewRequestWithContext(withRedirectState(ctx, st), http.MethodGet, robotsURL, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("User-Agent", r.userAgent)
	req.Header.Set("Accept", "text/plain,*/*;q=0.5")
	req.Header.Set("Accept-Encoding", "identity")

	resp, err := r.client.Do(req)
	if err != nil {
		// A denied address is a policy decision, not a transient fault: there is no
		// robots.txt to read and there never will be.
		if errors.Is(err, ErrBlockedAddress) || errors.Is(err, ErrBlockedHost) || errors.Is(err, ErrBlockedPort) {
			return nil, false, err
		}
		return nil, false, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
	}()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, r.maxBytes))
		if rerr != nil {
			return nil, false, rerr
		}
		return parseRobots(body), true, nil
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		// RFC 9309 §2.3.1.3: "unavailable" status means the crawler may access any
		// resource. A host with no robots.txt has published no restrictions.
		return &robotsFile{}, true, nil
	default:
		// 5xx and 3xx that were not followed: the policy is unknown.
		return nil, false, &robotsUnavailableError{status: resp.StatusCode}
	}
}

type robotsUnavailableError struct{ status int }

func (e *robotsUnavailableError) Error() string {
	return "fetch: robots.txt unavailable (status " + strconv.Itoa(e.status) + ")"
}

// parseRobots parses a robots.txt body.
//
// The grammar is small and the traps are all in group handling: consecutive
// User-agent lines share one group, and a User-agent line following a rule starts a new
// one. Getting that wrong silently applies another crawler's rules to FirmScout.
func parseRobots(body []byte) *robotsFile {
	file := &robotsFile{}
	var cur *robotsGroup
	// sawRule reports whether a rule has been seen since the last User-agent line, which
	// is what separates "another agent for this group" from "a new group".
	sawRule := false

	for _, raw := range strings.Split(string(body), "\n") {
		line := raw
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" {
			continue
		}
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		field := strings.ToLower(strings.TrimSpace(line[:colon]))
		value := strings.TrimSpace(line[colon+1:])

		switch field {
		case "user-agent", "useragent":
			if cur == nil || sawRule {
				file.groups = append(file.groups, robotsGroup{})
				cur = &file.groups[len(file.groups)-1]
				sawRule = false
			}
			if value != "" {
				cur.agents = append(cur.agents, strings.ToLower(value))
			}
		case "disallow", "allow":
			if cur == nil {
				continue // a rule outside any group has no agent to apply to
			}
			sawRule = true
			if value == "" {
				// "Disallow:" with an empty value means no restriction; an empty
				// Allow is equally meaningless. Both match nothing.
				continue
			}
			cur.rules = append(cur.rules, robotsRule{allow: field == "allow", pattern: value})
		case "crawl-delay":
			if cur == nil {
				continue
			}
			sawRule = true
			if secs, err := strconv.ParseFloat(value, 64); err == nil && secs >= 0 {
				cur.crawlDelay = time.Duration(secs * float64(time.Second))
				cur.hasCrawlDelay = true
			}
		case "sitemap":
			file.sitemaps = append(file.sitemaps, value)
		}
	}
	return file
}

// productToken reduces a User-Agent header to the product token robots.txt groups are
// matched against: "FirmScout/0.1 (+https://...)" becomes "firmscout".
func productToken(ua string) string {
	ua = strings.TrimSpace(ua)
	if ua == "" {
		return ""
	}
	if i := strings.IndexAny(ua, "/ \t"); i > 0 {
		ua = ua[:i]
	}
	return strings.ToLower(ua)
}

// selectGroup returns the most specific applicable group, falling back to "*".
//
// Specificity is the length of the matching agent token, per RFC 9309 §2.2.1: a group
// for "firmscout" beats one for "firm", and both beat "*".
func (f *robotsFile) selectGroup(token string) *robotsGroup {
	var best *robotsGroup
	bestLen := -1
	var wildcard *robotsGroup

	for i := range f.groups {
		g := &f.groups[i]
		for _, agent := range g.agents {
			if agent == "*" {
				if wildcard == nil {
					wildcard = g
				}
				continue
			}
			if token == "" || !strings.HasPrefix(token, agent) {
				continue
			}
			if len(agent) > bestLen {
				best, bestLen = g, len(agent)
			}
		}
	}
	if best != nil {
		return best
	}
	return wildcard
}

// allows applies the longest-match-wins rule between Allow and Disallow.
//
// Ties go to Allow, which is the behaviour RFC 9309 §2.2.2 specifies and the behaviour
// site operators expect when they write a broad Disallow with a narrow Allow carve-out.
func (g *robotsGroup) allows(path string) bool {
	longestAllow, longestDisallow := -1, -1
	for _, rule := range g.rules {
		if !robotsPathMatch(rule.pattern, path) {
			continue
		}
		n := len(rule.pattern)
		if rule.allow {
			if n > longestAllow {
				longestAllow = n
			}
		} else if n > longestDisallow {
			longestDisallow = n
		}
	}
	if longestDisallow < 0 {
		return true
	}
	return longestAllow >= longestDisallow
}

// robotsPathMatch matches a robots.txt path pattern against a path, supporting the two
// wildcards in common use: "*" for any sequence and a trailing "$" anchoring the end.
func robotsPathMatch(pattern, path string) bool {
	if pattern == "" {
		return false
	}
	anchored := strings.HasSuffix(pattern, "$")
	if anchored {
		pattern = pattern[:len(pattern)-1]
	}
	segments := strings.Split(pattern, "*")

	pos := 0
	for i, seg := range segments {
		if seg == "" {
			continue
		}
		switch {
		case i == 0:
			// The first segment is anchored at the start of the path.
			if !strings.HasPrefix(path[pos:], seg) {
				return false
			}
			pos += len(seg)
		case anchored && i == len(segments)-1:
			// The last segment of an anchored pattern must end the path.
			if len(path)-pos < len(seg) || !strings.HasSuffix(path, seg) {
				return false
			}
			pos = len(path)
		default:
			idx := strings.Index(path[pos:], seg)
			if idx < 0 {
				return false
			}
			pos += idx + len(seg)
		}
	}
	if anchored && pos != len(path) {
		// Trailing "*" before "$" absorbs the remainder.
		return strings.HasSuffix(pattern, "*")
	}
	return true
}
