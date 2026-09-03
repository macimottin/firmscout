package apptest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/domain"
)

// Fetcher returns scripted responses so a use-case test never touches the network.
type Fetcher struct {
	// Responses is consumed in order; the last one repeats once exhausted.
	Responses []application.FetchResult
	Requests  []application.FetchRequest
	n         int
}

// Fetch returns the next scripted response.
func (f *Fetcher) Fetch(ctx context.Context, req application.FetchRequest) (application.FetchResult, error) {
	f.Requests = append(f.Requests, req)
	if len(f.Responses) == 0 {
		return application.FetchResult{Outcome: domain.OutcomeUnavailable}, fmt.Errorf("apptest: no scripted response")
	}
	i := f.n
	if i >= len(f.Responses) {
		i = len(f.Responses) - 1
	}
	f.n++
	res := f.Responses[i]
	return res, res.Err
}

// Normalizer strips nothing and hashes the body. It is enough to prove that the
// pipeline uses the hash to decide whether a change is real; the real normaliser's own
// tests cover stripping and section selection.
type Normalizer struct {
	// Sections maps a body to the bytes that should be hashed, letting a test
	// simulate "the page changed but the monitored section did not".
	Sections map[string]string
	Calls    int
}

// Normalize returns canonical bytes and their hash.
func (n *Normalizer) Normalize(contentType string, body []byte, sectionSelector string, strip []string) ([]byte, string, error) {
	n.Calls++
	content := string(body)
	if sectionSelector != "" && n.Sections != nil {
		if sec, ok := n.Sections[content]; ok {
			content = sec
		}
	}
	content = strings.Join(strings.Fields(content), " ")
	sum := sha256.Sum256([]byte(content))
	return []byte(content), hex.EncodeToString(sum[:]), nil
}

// Collector returns scripted candidates.
type Collector struct {
	CollectorID string
	Ver         string
	VendorSlug  string
	Candidates  []domain.CandidateRelease
	Err         error
	Calls       int
}

func (c *Collector) ID() string      { return c.CollectorID }
func (c *Collector) Version() string { return c.Ver }
func (c *Collector) Vendor() string  { return c.VendorSlug }

// Supports accepts any source, because a fake collector's job is to be selected.
func (c *Collector) Supports(src domain.Source) bool { return true }

// Extract returns the scripted candidates. Note the signature takes no repository and
// no clock, which is the property that makes real collectors unable to publish.
func (c *Collector) Extract(ctx context.Context, src domain.Source, art application.Artifact) ([]domain.CandidateRelease, error) {
	c.Calls++
	if c.Err != nil {
		return nil, c.Err
	}
	out := make([]domain.CandidateRelease, len(c.Candidates))
	copy(out, c.Candidates)
	return out, nil
}

// Registry resolves every source to one collector.
type Registry struct{ C application.Collector }

// For returns the single registered collector.
func (r *Registry) For(src domain.Source) (application.Collector, error) {
	if r.C == nil {
		return nil, fmt.Errorf("apptest: no collector registered for source %s: %w", src.ID, domain.ErrNotFound)
	}
	return r.C, nil
}

// All returns the registered collectors.
func (r *Registry) All() []application.Collector {
	if r.C == nil {
		return nil
	}
	return []application.Collector{r.C}
}
