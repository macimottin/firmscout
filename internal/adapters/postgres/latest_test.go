package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/macimottin/firmscout/internal/domain"
)

// These are the revert detectors for the "what version should I be on?" path. Each one
// reproduces a defect that was measured against a real database, and each fails if the
// repair is taken out again.

// latestFixture inserts one release per channel, each carrying the latest-observed flag
// for its own channel, and returns them in the order given.
//
// Insertion order is deliberately the reverse of the answer: the release that must win
// is written last, so a query that lost its ORDER BY and fell back to whatever the heap
// hands over first returns the wrong row rather than accidentally the right one.
func latestFixture(t *testing.T, db *DB, vendorID, evidenceID, productID string, specs []latestSpec) []domain.Release {
	t.Helper()
	ctx := context.Background()
	releases := NewReleaseRepo(db)

	out := make([]domain.Release, 0, len(specs))
	for _, s := range specs {
		rel := newRelease(t, s.id, vendorID, evidenceID, s.version, s.channel, s.observed)
		rel.ReleaseDate = s.date
		m := domain.ReleaseProductMapping{
			ID:               "rmap_" + s.id,
			ReleaseID:        rel.ID,
			ProductID:        productID,
			Applicability:    domain.Applicability{Channel: s.channel},
			IsLatestObserved: true,
		}
		if err := releases.Insert(ctx, rel, []domain.ReleaseProductMapping{m}); err != nil {
			t.Fatalf("Insert %s: %v", s.id, err)
		}
		out = append(out, rel)
	}
	return out
}

// latestSpec is one release in a latestFixture. The channel is carried on both the
// release and its mapping, the way the publication use case writes them.
type latestSpec struct {
	id       string
	version  string
	channel  string
	date     domain.PartialDate
	observed time.Time
}

// TestLatestForProductWithoutAChannelAnswersAcrossChannels is the revert detector for
// the 404 that GET /products/{slug}/latest returned to its own documented default call.
//
// openapi.yaml documents ?channel as optional. The predicate compared the parameter for
// equality against the mapping's channel coalesced to the empty string, so an omitted
// channel asked for a mapping carrying no channel at all -- and every product whose
// releases are all channelled has none. Against the dev database that was RouterOS with
// 44 published releases: 404 without ?channel, 200 with it. The fixture below is the
// same shape in miniature, and it fails the moment the parameter is treated as an
// equality test again.
//
// The second half of the test is what stops the fix from going too far: asking for a
// channel must still restrict to that channel, including when the channel asked for is
// not the one that would win an unrestricted call.
func TestLatestForProductWithoutAChannelAnswersAcrossChannels(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	v, p := seedCatalog(t, db)
	e := seedEvidence(t, db, v.ID)
	releases := NewReleaseRepo(db)

	now := time.Now().UTC()
	latestFixture(t, db, v.ID, e.ID, p.ID, []latestSpec{
		{"rel_lt", "6.49.18", "long_term", mustExactDate(t, 2026, time.January, 10), now.Add(-time.Hour)},
		{"rel_stable", "7.24.2", "stable", mustExactDate(t, 2026, time.February, 14), now.Add(-2 * time.Hour)},
	})

	got, err := releases.LatestForProduct(ctx, p.ID, "")
	if err != nil {
		t.Fatalf("LatestForProduct with no channel = %v; the endpoint's own default call "+
			"must answer, not 404 -- an omitted channel means any channel, not the "+
			"channel whose name is empty", err)
	}
	if got.ID != "rel_stable" {
		t.Errorf("LatestForProduct with no channel = %q, want rel_stable, the newest by "+
			"release date across every channel", got.ID)
	}

	perChannel, err := releases.LatestForProduct(ctx, p.ID, "long_term")
	if err != nil {
		t.Fatalf("LatestForProduct(long_term): %v", err)
	}
	if perChannel.ID != "rel_lt" {
		t.Errorf("LatestForProduct(long_term) = %q, want rel_lt; a channel that is asked "+
			"for must still restrict the answer to that channel", perChannel.ID)
	}

	if _, err := releases.LatestForProduct(ctx, p.ID, "no_such_channel"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("LatestForProduct on an unknown channel = %v, want domain.ErrNotFound", err)
	}
}

// TestLatestForProductOrdersLikeDomainLatestComparison pins the one ordering rule to
// its three spellings.
//
// The rule is written three times: domain.LatestComparison in Go, which the publication
// use case applies when it sets is_latest_observed; latestOrdering in SQL, which
// LatestForProduct applies to choose between the channels that each hold a flag; and
// latestOrdering again in RefreshProductSummary, which fills the headline version on the
// product page. The endpoint and the page answer the same question for the same reader,
// so a disagreement between them has no tie-breaker: the page would show one version
// while the API told the same fleet to install another, each internally consistent, with
// nothing in either response admitting a second answer exists. Before the repair those
// two really did ask different questions -- across all channels and per channel.
//
// The fixture separates the three orderings a reader might mistake for each other. The
// release that must win is the newest by release date but the *oldest* observation, and
// the most recent observation of the three has no release date at all -- so ordering by
// first_observed_at, or putting NULL dates first, or dropping the ORDER BY and taking
// whatever the heap yields, each produce a different, wrong answer.
func TestLatestForProductOrdersLikeDomainLatestComparison(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	v, p := seedCatalog(t, db)
	e := seedEvidence(t, db, v.ID)
	releases := NewReleaseRepo(db)
	summaries := NewSummaryRepo(db)

	now := time.Now().UTC()
	inserted := latestFixture(t, db, v.ID, e.ID, p.ID, []latestSpec{
		{"rel_testing", "7.25beta1", "testing", domain.UnknownDate, now},
		{"rel_lt", "6.49.18", "long_term", mustExactDate(t, 2026, time.January, 10), now.Add(-time.Hour)},
		{"rel_stable", "7.24.2", "stable", mustExactDate(t, 2026, time.February, 14), now.Add(-2 * time.Hour)},
	})

	// The expected answer is not written down as a literal; it is folded out of the
	// domain rule itself, so this test cannot drift from domain.LatestComparison even
	// if the fixture is rewritten.
	want := inserted[0]
	for _, rel := range inserted[1:] {
		if domain.LatestComparison(rel, want) > 0 {
			want = rel
		}
	}
	if want.ID != "rel_stable" {
		t.Fatalf("the fixture no longer exercises the rule: domain.LatestComparison picks %q", want.ID)
	}

	got, err := releases.LatestForProduct(ctx, p.ID, "")
	if err != nil {
		t.Fatalf("LatestForProduct: %v", err)
	}
	if got.ID != want.ID {
		t.Errorf("LatestForProduct = %q but domain.LatestComparison says %q; the SQL "+
			"ordering and the Go rule have drifted, and the flag the publication use "+
			"case sets no longer means what this query reads", got.ID, want.ID)
	}

	if err := releases.RefreshProductSummary(ctx, p.ID); err != nil {
		t.Fatalf("RefreshProductSummary: %v", err)
	}
	sum, err := summaries.Get(ctx, p.Slug)
	if err != nil {
		t.Fatalf("summary Get: %v", err)
	}
	if sum.LatestReleaseID != got.ID {
		t.Errorf("the product page's headline latest is %q and /latest answers %q for the "+
			"same product at the same moment; a fleet manager reading the page and a "+
			"client calling the API would install different firmware and neither answer "+
			"would say the other exists", sum.LatestReleaseID, got.ID)
	}
	if sum.LatestRawVersion != got.Version.Raw() {
		t.Errorf("summary latest version = %q, /latest = %q", sum.LatestRawVersion, got.Version.Raw())
	}
}

// TestLatestForProductNeverServesAWithdrawnRelease is the revert detector for the worst
// answer this API can give.
//
// A withdrawn release is one the vendor pulled. Offering it as the answer to "what
// version should I be on?" repeats a retraction as advice. The query had no withdrawn
// predicate at all, so a release withdrawn after it was flagged latest kept being served
// as the recommendation -- while the product summary, which does exclude withdrawn rows,
// correctly reported no latest release for the same product. Both halves are asserted
// here, because the disagreement between them was the visible symptom.
//
// The withdrawal is written straight through the pool. Withdrawing a release is not an
// operation this repository offers -- releases are append-only and TouchVerified is the
// only UPDATE in the package -- and the point of the test is the state, not the route to
// it.
//
// The second half is what proves the SQL predicate is load-bearing rather than the
// Serveable check alone: a serveable release exists in another channel, and it is the
// one that must come back. A query that selects the withdrawn row first and only then
// notices it is unserveable has nothing left to fall back to and answers not-found for a
// product that has a perfectly good answer.
func TestLatestForProductNeverServesAWithdrawnRelease(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	v, p := seedCatalog(t, db)
	e := seedEvidence(t, db, v.ID)
	releases := NewReleaseRepo(db)
	summaries := NewSummaryRepo(db)

	now := time.Now().UTC()
	latestFixture(t, db, v.ID, e.ID, p.ID, []latestSpec{
		{"rel_pulled", "7.24.2", "stable", mustExactDate(t, 2026, time.February, 14), now},
	})
	if _, err := pool(db).Exec(ctx,
		`UPDATE releases SET withdrawn = true, withdrawn_at = now(),
             withdrawn_reason = 'the vendor pulled the image' WHERE id = 'rel_pulled'`); err != nil {
		t.Fatalf("withdraw rel_pulled: %v", err)
	}

	if _, err := releases.LatestForProduct(ctx, p.ID, ""); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("LatestForProduct served a withdrawn release as the answer to \"what "+
			"version should I be on?\" (%v); the vendor pulled it", err)
	}
	if _, err := releases.LatestForProduct(ctx, p.ID, "stable"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("LatestForProduct(stable) served a withdrawn release: %v", err)
	}

	if err := releases.RefreshProductSummary(ctx, p.ID); err != nil {
		t.Fatalf("RefreshProductSummary: %v", err)
	}
	sum, err := summaries.Get(ctx, p.Slug)
	if err != nil {
		t.Fatalf("summary Get: %v", err)
	}
	if sum.LatestReleaseID != "" {
		t.Errorf("the summary reports %q as the latest release although it is withdrawn", sum.LatestReleaseID)
	}

	// A serveable release in another channel is the answer, and it must survive the
	// withdrawn one being both newer and flagged.
	latestFixture(t, db, v.ID, e.ID, p.ID, []latestSpec{
		{"rel_lt", "6.49.18", "long_term", mustExactDate(t, 2026, time.January, 10), now.Add(-time.Hour)},
	})
	got, err := releases.LatestForProduct(ctx, p.ID, "")
	if err != nil {
		t.Fatalf("LatestForProduct with a withdrawn flagged release and a serveable one = %v; "+
			"the serveable release is the answer", err)
	}
	if got.ID != "rel_lt" {
		t.Errorf("LatestForProduct = %q, want rel_lt", got.ID)
	}
}

// TestSearchAcceptsAnOverlongMultibyteQuery is the revert detector for a 500 that any
// caller could trigger by pasting a model name.
//
// The truncation to MaxSearchQueryLength was a raw byte slice. A query longer than the
// bound whose bytes do not happen to align with it was cut through the middle of a rune,
// and the orphaned continuation bytes are not valid UTF-8; PostgreSQL rejects invalid
// UTF-8, so websearch_to_tsquery and similarity() failed the statement. The result was
// an error instead of a shorter search, for an accented or CJK model name out of an
// asset register -- input a catalogue of hardware from every vendor must expect.
//
// Both fixtures are chosen so the byte bound lands inside a rune: the ASCII prefix makes
// the two-byte case cut at an odd offset, and three-byte runes never align with 200.
func TestSearchAcceptsAnOverlongMultibyteQuery(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	summaries := NewSummaryRepo(db)

	cases := []struct {
		name  string
		query string
	}{
		{"two-byte runes cut at an odd offset", "x" + strings.Repeat("\u00e9", MaxSearchQueryLength+1)},
		{"three-byte runes never align with the bound", strings.Repeat("\u5149", MaxSearchQueryLength+1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := summaries.Search(ctx, tc.query, 10); err != nil {
				t.Fatalf("Search on a %d-byte query = %v; an over-long query must be "+
					"truncated to a shorter search, never cut into invalid UTF-8 that "+
					"the database refuses", len(tc.query), err)
			}
		})
	}
}
