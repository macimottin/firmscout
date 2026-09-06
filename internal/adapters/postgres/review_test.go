package postgres

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/domain"
)

// reviewQueueItem builds a review item with the fields List's ordering and filters
// exercise. It stays local to this file rather than joining postgres_test.go's shared
// applicationReviewItem, which fixes every field but the id.
func reviewQueueItem(id, kind, sla string, priority int, vendorID, productID string, createdAt time.Time) application.ReviewItem {
	return application.ReviewItem{
		ID:            id,
		Kind:          kind,
		SubjectType:   "candidate_release",
		SubjectID:     "cand_" + id,
		VendorID:      vendorID,
		ProductID:     productID,
		Title:         "title for " + id,
		PriorityScore: priority,
		SLAClass:      sla,
		CreatedAt:     createdAt,
	}
}

func idsOf(items []application.ReviewItem) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.ID
	}
	return out
}

// TestReviewListFiltersAndPages proves List orders the way review_items_queue_idx does
// (priority_score DESC, created_at ASC, id ASC), that kind, SLA class, vendor and state
// filters narrow the set independently, and that a caller paging through with Limit set
// sees every item exactly once -- no overlap, no gap -- against the unpaged order.
func TestReviewListFiltersAndPages(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	v, p := seedCatalog(t, db)
	reviews := NewReviewRepo(db)

	other := newVendor("ven_other", "othervendor")
	if err := NewVendorRepo(db).Upsert(ctx, other); err != nil {
		t.Fatalf("seed other vendor: %v", err)
	}
	otherProduct := newProduct("prd_other", other.ID, "other-product")
	if err := NewProductRepo(db).Upsert(ctx, otherProduct); err != nil {
		t.Fatalf("seed other product: %v", err)
	}

	base := time.Now().UTC().Add(-time.Hour)
	items := []application.ReviewItem{
		reviewQueueItem("rev_1", "multi_source_conflict", "urgent", 220, v.ID, p.ID, base),
		reviewQueueItem("rev_2", "multi_source_conflict", "high", 180, v.ID, p.ID, base.Add(time.Minute)),
		// Same priority: created_at ascending breaks the tie.
		reviewQueueItem("rev_3", "product_match_ambiguous", "standard", 140, v.ID, p.ID, base.Add(2*time.Minute)),
		reviewQueueItem("rev_5", "product_match_ambiguous", "standard", 140, v.ID, p.ID, base.Add(4*time.Minute)),
		reviewQueueItem("rev_4", "candidate_low_confidence", "low", 100, v.ID, p.ID, base.Add(3*time.Minute)),
		// A different vendor entirely, highest priority of all: the vendor filter must
		// be able to exclude it even though it would otherwise sort first.
		reviewQueueItem("rev_6", "multi_source_conflict", "urgent", 999, other.ID, otherProduct.ID, base.Add(5*time.Minute)),
	}
	for _, it := range items {
		if err := reviews.Create(ctx, it); err != nil {
			t.Fatalf("seed %s: %v", it.ID, err)
		}
	}

	wantVendorOrder := []string{"rev_1", "rev_2", "rev_3", "rev_5", "rev_4"}

	t.Run("vendor filter and full ordering", func(t *testing.T) {
		got, next, err := reviews.List(ctx, application.ReviewQueueFilter{VendorID: v.ID, Limit: 10})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if next != "" {
			t.Errorf("next cursor = %q, want empty: the page held every matching item", next)
		}
		if !reflect.DeepEqual(idsOf(got), wantVendorOrder) {
			t.Errorf("order = %v, want %v", idsOf(got), wantVendorOrder)
		}
	})

	t.Run("kind filter", func(t *testing.T) {
		got, _, err := reviews.List(ctx, application.ReviewQueueFilter{Kinds: []string{"multi_source_conflict"}, VendorID: v.ID, Limit: 10})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if want := []string{"rev_1", "rev_2"}; !reflect.DeepEqual(idsOf(got), want) {
			t.Errorf("kind-filtered order = %v, want %v", idsOf(got), want)
		}
	})

	t.Run("SLA class filter", func(t *testing.T) {
		got, _, err := reviews.List(ctx, application.ReviewQueueFilter{SLAClasses: []string{"standard"}, VendorID: v.ID, Limit: 10})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if want := []string{"rev_3", "rev_5"}; !reflect.DeepEqual(idsOf(got), want) {
			t.Errorf("SLA-filtered order = %v, want %v", idsOf(got), want)
		}
	})

	t.Run("pagination has no overlap and no gap", func(t *testing.T) {
		var paged []string
		cursor := ""
		for i := 0; i < 10; i++ {
			page, next, err := reviews.List(ctx, application.ReviewQueueFilter{VendorID: v.ID, Limit: 2, Cursor: cursor})
			if err != nil {
				t.Fatalf("List page %d: %v", i, err)
			}
			paged = append(paged, idsOf(page)...)
			if next == "" {
				break
			}
			cursor = next
		}
		if !reflect.DeepEqual(paged, wantVendorOrder) {
			t.Errorf("paged order = %v, want %v", paged, wantVendorOrder)
		}
	})

	t.Run("resolved items are excluded by default and included on request", func(t *testing.T) {
		if err := reviews.Resolve(ctx, "rev_4", "duplicate of a published release", "maintainer", time.Now().UTC()); err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		open, _, err := reviews.List(ctx, application.ReviewQueueFilter{VendorID: v.ID, Limit: 10})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, it := range open {
			if it.ID == "rev_4" {
				t.Fatal("a resolved item appeared in the default (open, in_progress) queue")
			}
		}
		withResolved, _, err := reviews.List(ctx, application.ReviewQueueFilter{
			VendorID: v.ID, Limit: 10,
			States: []string{application.ReviewStateOpen, application.ReviewStateInProgress, application.ReviewStateResolved},
		})
		if err != nil {
			t.Fatalf("List with resolved included: %v", err)
		}
		found := false
		for _, it := range withResolved {
			if it.ID == "rev_4" {
				found = true
				if it.State != application.ReviewStateResolved || it.ResolvedBy != "maintainer" {
					t.Errorf("resolved item = %+v, want state resolved / resolved_by maintainer", it)
				}
			}
		}
		if !found {
			t.Error("an explicit States filter that includes resolved did not return the resolved item")
		}
	})

	t.Run("GetByID matches List", func(t *testing.T) {
		got, err := reviews.GetByID(ctx, "rev_1")
		if err != nil {
			t.Fatalf("GetByID: %v", err)
		}
		if got.Kind != "multi_source_conflict" || got.PriorityScore != 220 {
			t.Errorf("GetByID = %+v", got)
		}
		if _, err := reviews.GetByID(ctx, "rev_missing"); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("GetByID on a missing item = %v, want domain.ErrNotFound", err)
		}
	})
}

// TestAuditRecordStoresUnauthenticatedActor is the revert detector for D9's schema
// half: every row this phase writes asserts actor_authenticated = false, and it must
// round-trip as false rather than as the column's bare default happening to agree.
func TestAuditRecordStoresUnauthenticatedActor(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	audit := NewAuditRepo(db)

	at := time.Now().UTC()
	event := application.AuditEvent{
		ID:                 "aud_1",
		ActorType:          application.ActorTypeHuman,
		ActorID:            "reviewer@example.test",
		ActorAuthenticated: false,
		Action:             application.AuditActionReviewAccepted,
		SubjectType:        "review_item",
		SubjectID:          "rev_1",
		BeforeState:        map[string]string{"review_state": "open"},
		AfterState:         map[string]string{"review_state": "resolved", "release_id": "rel_1"},
		Reason:             "matches the authoritative changelog",
		OccurredAt:         at,
	}
	if err := audit.Record(ctx, event); err != nil {
		t.Fatalf("Record: %v", err)
	}

	got, err := audit.ListForSubject(ctx, "review_item", "rev_1", 10)
	if err != nil {
		t.Fatalf("ListForSubject: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("audit events = %d, want 1", len(got))
	}
	if got[0].ActorAuthenticated {
		t.Error("ActorAuthenticated round-tripped as true; every row this phase writes must be honestly false")
	}
	if got[0].ActorID != "reviewer@example.test" || got[0].Action != application.AuditActionReviewAccepted {
		t.Errorf("round trip lost data: %+v", got[0])
	}
	if got[0].AfterState["release_id"] != "rel_1" {
		t.Errorf("AfterState = %v, want release_id rel_1", got[0].AfterState)
	}

	// Read the raw column too: the point of this test is that the database, not just
	// the Go struct, holds false.
	var stored bool
	if err := pool(db).QueryRow(ctx,
		`SELECT actor_authenticated FROM audit_events WHERE id = $1`, "aud_1").Scan(&stored); err != nil {
		t.Fatalf("read raw column: %v", err)
	}
	if stored {
		t.Error("actor_authenticated stored as true; every write this phase makes must be honestly false")
	}

	// A second, differently-scoped event proves ListForSubject actually filters by
	// subject rather than returning everything.
	if err := audit.Record(ctx, application.AuditEvent{
		ID: "aud_2", ActorType: application.ActorTypeSystem, Action: "publication.requested",
		SubjectType: "candidate_release", SubjectID: "cand_1", OccurredAt: at,
	}); err != nil {
		t.Fatalf("Record second event: %v", err)
	}
	stillOne, err := audit.ListForSubject(ctx, "review_item", "rev_1", 10)
	if err != nil {
		t.Fatalf("ListForSubject: %v", err)
	}
	if len(stillOne) != 1 {
		t.Errorf("ListForSubject returned %d events, want 1 scoped to review_item/rev_1", len(stillOne))
	}
}
