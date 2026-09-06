package platform

import (
	"context"
	"testing"
	"time"

	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/application/apptest"
	"github.com/macimottin/firmscout/internal/domain"
)

// stubVendors exists because apptest ships no vendor fake and this test needs a
// non-nil value, not a working one: what is under test is whether the composition root
// passes the port along, never what the port returns.
type stubVendors struct{}

func (stubVendors) GetByID(context.Context, string) (domain.Vendor, error) {
	return domain.Vendor{}, domain.ErrNotFound
}
func (stubVendors) GetBySlug(context.Context, string) (domain.Vendor, error) {
	return domain.Vendor{}, domain.ErrNotFound
}
func (stubVendors) List(context.Context, int, string) ([]domain.Vendor, string, error) {
	return nil, "", nil
}
func (stubVendors) Upsert(context.Context, domain.Vendor) error { return nil }

// TestEveryPortReachesTheUseCaseThatNeedsIt guards the composition root's one failure
// mode: a port that is present on the Container and silently absent from the dependency
// struct built out of it.
//
// The failure is invisible by construction. ValidateCandidate tolerates a nil Conflicts
// port -- it has to, because the projection did not exist before Phase 2 -- and a
// deployment missing it does not error. It publishes a version two sources disagree
// about and records "all gates passed", which is the one outcome ADR-0020 exists to
// prevent. Nothing else in the test suite can see that, because every other test builds
// its own dependency struct and therefore cannot forget a field this file forgot.
func TestEveryPortReachesTheUseCaseThatNeedsIt(t *testing.T) {
	conflicts := apptest.NewConflicts()
	audit := apptest.NewAudit()
	reviews := &apptest.Reviews{}
	c := &Container{
		Vendors:    stubVendors{},
		Products:   apptest.NewProducts(),
		Sources:    apptest.NewSources(),
		Candidates: apptest.NewCandidates(),
		Releases:   apptest.NewReleases(),
		Evidence:   apptest.NewEvidenceStore(),
		Reviews:    reviews,
		Conflicts:  conflicts,
		Audit:      audit,
		Clock:      apptest.NewClock(time.Unix(0, 0).UTC()),
		IDs:        apptest.NewIDGen(),
	}

	ingest := c.IngestDeps()
	if ingest.Conflicts == nil {
		t.Error("IngestDeps drops the Conflicts port; gate 10 would then be unable to see any disagreement")
	}
	if ingest.Audit == nil {
		t.Error("IngestDeps drops the Audit port")
	}

	read := c.ReviewQueryDeps()
	for name, port := range map[string]any{
		"Reviews": read.Reviews, "Candidates": read.Candidates, "Evidence": read.Evidence,
		"Sources": read.Sources, "Products": read.Products, "Vendors": read.Vendors,
		"Conflicts": read.Conflicts, "Audit": read.Audit, "Clock": read.Clock,
	} {
		if port == nil {
			t.Errorf("ReviewQueryDeps drops the %s port; the reviewer's page loses that panel silently", name)
		}
	}

	publisher := application.NewPublishRelease(ingest)
	write := c.ReviewDeps(publisher)
	if write.Publisher != publisher {
		t.Error("ReviewDeps built its own publisher; a hand-approved release must be produced the same way an automatic one is")
	}
	for name, port := range map[string]any{
		"Reviews": write.Reviews, "Candidates": write.Candidates,
		"Conflicts": write.Conflicts, "Audit": write.Audit,
		"Clock": write.Clock, "IDs": write.IDs,
	} {
		if port == nil {
			t.Errorf("ReviewDeps drops the %s port", name)
		}
	}
}

// TestTheReviewAPIIsOffUnlessSomebodySaysOtherwise. The surface performs writes on the
// word of an actor nobody authenticated, so an unset variable must never reach it.
func TestTheReviewAPIIsOffUnlessSomebodySaysOtherwise(t *testing.T) {
	t.Setenv("FIRMSCOUT_DATABASE_URL", "postgres://user:pass@localhost:5432/firmscout")
	t.Setenv("FIRMSCOUT_REVIEW_API_ENABLED", "")
	cfg, err := Load("firmscout-api")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ReviewAPIEnabled {
		t.Fatal("the internal review API is enabled by default; it performs unauthenticated writes (ADR-0021)")
	}

	t.Setenv("FIRMSCOUT_REVIEW_API_ENABLED", "true")
	cfg, err = Load("firmscout-api")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.ReviewAPIEnabled {
		t.Error("FIRMSCOUT_REVIEW_API_ENABLED=true did not enable the review API")
	}
}
