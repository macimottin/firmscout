package application

import (
	"context"
	"fmt"
	"time"

	"github.com/macimottin/firmscout/internal/domain"
)

// ConflictQueryDeps are the ports the open-conflict read side needs. Like
// ReviewQueryDeps they are read-only: nothing in this file writes, which is what would
// let this surface be served from a replica later without revisiting the question.
type ConflictQueryDeps struct {
	Conflicts ConflictRepository
	Products  ProductRepository
	Vendors   VendorRepository
	Reviews   ReviewRepository
	Clock     Clock
}

// Open-conflict page bounds. They are separate constants from the review queue's because
// the two surfaces are read for different reasons: the review queue is worked through
// item by item, while the conflict list is scanned to see whether the detector is
// behaving at all.
const (
	DefaultOpenConflictPageSize = 50
	MaxOpenConflictPageSize     = 500
)

// OpenConflictEntry is one unresolved disagreement with the names a human needs to
// recognise it and the queue item that covers it.
type OpenConflictEntry struct {
	Conflict domain.SourceConflict

	VendorSlug  string
	VendorName  string
	ProductSlug string
	ProductName string

	// Observations is what each source claims for the disputed product and channel right
	// now, read live rather than from the conflict row: the row records who disagreed
	// when it opened, and a maintainer looking today needs today's claims.
	Observations []domain.SourceObservation

	// ReviewItem is the queue item covering this conflict. Queued reports whether there
	// is one, because a conflict with no queue item is not a cosmetic gap -- it is the
	// exact failure this surface exists to make visible, and rendering an empty column
	// would read as a missing join rather than as the fact it is.
	ReviewItem ReviewItem
	Queued     bool

	// Age is how long the disagreement has been open, computed at read time.
	Age time.Duration
}

// ListOpenConflicts serves the unresolved multi-source disagreements.
//
// It exists because ConflictRepository.ListOpenConflicts and OpenConflictFor were
// implemented and tested and then wired to nothing: no use case, no route, no command.
// ADR-0020's whole premise is that a disagreement between sources of equal authority is
// never auto-resolved but reaches a human instead, and a disagreement recorded in a table
// nothing reads has not reached anybody. The review queue covers the ordinary path; this
// is the answer to "is the detector finding things, and is every one of them queued".
type ListOpenConflicts struct {
	deps ConflictQueryDeps
}

// NewListOpenConflicts builds the use case.
func NewListOpenConflicts(d ConflictQueryDeps) *ListOpenConflicts {
	return &ListOpenConflicts{deps: d}
}

// Execute returns the open conflicts, most recently detected first.
//
// A missing vendor, product or review item is never an error: source_conflicts.product_id
// cascades and review_item_id is ON DELETE SET NULL, so a conflict can outlive either.
// Only the conflicts themselves are required.
func (uc *ListOpenConflicts) Execute(ctx context.Context, limit int) ([]OpenConflictEntry, error) {
	// Out of range in each direction is answered differently on purpose, matching
	// ListReleases and ListReviewQueue: absent means "give me the documented default",
	// while too large means "give me as much as you will allow". Collapsing an
	// over-large ask to the default hands an operator running `firmscout conflicts list
	// --limit 1000` fifty rows with nothing saying it truncated -- to someone whose
	// stated reason for running the command is to see whether the detector finds
	// anything at all.
	if limit <= 0 {
		limit = DefaultOpenConflictPageSize
	}
	if limit > MaxOpenConflictPageSize {
		limit = MaxOpenConflictPageSize
	}

	conflicts, err := uc.deps.Conflicts.ListOpenConflicts(ctx, limit)
	if err != nil {
		return nil, fmt.Errorf("list open conflicts: %w", err)
	}

	now := uc.deps.Clock.Now()
	out := make([]OpenConflictEntry, 0, len(conflicts))
	for _, conflict := range conflicts {
		entry := OpenConflictEntry{
			Conflict: conflict,
			Age:      queueAge(now, conflict.DetectedAt),
		}

		product, err := uc.deps.Products.GetByID(ctx, conflict.ProductID)
		switch {
		case err == nil:
			entry.ProductSlug = product.Slug
			entry.ProductName = product.Name
			if vendor, verr := uc.deps.Vendors.GetByID(ctx, product.VendorID); verr == nil {
				entry.VendorSlug = vendor.Slug
				entry.VendorName = vendor.Name
			} else if !isNotFound(verr) {
				return nil, fmt.Errorf("resolve vendor %s: %w", product.VendorID, verr)
			}
		case isNotFound(err):
		default:
			return nil, fmt.Errorf("resolve product %s: %w", conflict.ProductID, err)
		}

		obs, err := uc.deps.Conflicts.ObservationsForProduct(ctx, conflict.ProductID, conflict.Channel)
		if err != nil {
			return nil, fmt.Errorf("load observations for conflict %s: %w", conflict.ID, err)
		}
		entry.Observations = obs

		if conflict.ReviewItemID != "" {
			item, err := uc.deps.Reviews.GetByID(ctx, conflict.ReviewItemID)
			switch {
			case err == nil:
				entry.ReviewItem = item
				entry.Queued = true
			case isNotFound(err):
			default:
				return nil, fmt.Errorf("load review item %s: %w", conflict.ReviewItemID, err)
			}
		}

		out = append(out, entry)
	}
	return out, nil
}
