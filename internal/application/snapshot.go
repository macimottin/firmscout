package application

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/macimottin/firmscout/internal/domain"
)

// A snapshot is the catalogue's observed facts in a form that survives leaving the
// database: one published release per record, carrying the evidence that justifies it
// and naming everything else by slug.
//
// # Why slugs rather than identifiers
//
// Vendor, product and source identifiers are minted by `registry sync`, so two
// installations that sync the same committed registry end up with different ones. A
// snapshot that referenced them would import cleanly only into the database it was
// exported from, which defeats the point: the file exists so that somebody who clones
// this repository gets a populated catalogue without fetching anything from a
// manufacturer. Slugs are the registry's own stable names, so they mean the same thing
// in every installation, and resolving them on import is what makes the file portable.
//
// The release's own identifier is preserved rather than regenerated, because it is what
// makes re-importing idempotent and what lets an exported fact be traced back to the
// row it came from. It is the one identifier a snapshot carries.
//
// # What a snapshot deliberately leaves out
//
// Candidates, jobs, review items, source checks and usage records are working state,
// not facts about firmware: they describe what FirmScout was doing, not what a vendor
// published. Raw artifacts are excluded for two reasons that both matter -- they are
// hundreds of kilobytes of a manufacturer's own markup, and licensing.md is explicit
// that FirmScout links to release documents rather than reproducing them. What survives
// of an artifact is its content hash and the bounded excerpt already stored on the
// evidence, which is enough to verify a claim and not enough to republish a page.

// SnapshotVersion is the format's own version, written into the manifest.
//
// It is not the schema version. A migration that adds a column nothing exports does not
// change this; a change to what a record means does, so an importer can refuse a file
// it would misread rather than importing it into the wrong shape.
const SnapshotVersion = 1

// ReleaseFact is one published release with its provenance, as a snapshot record.
type ReleaseFact struct {
	Release  SnapshotRelease   `json:"release"`
	Vendor   string            `json:"vendor"`
	Products []SnapshotMapping `json:"products"`
	Evidence SnapshotEvidence  `json:"evidence"`
}

// SnapshotRelease is the release itself.
type SnapshotRelease struct {
	ID          string       `json:"id"`
	Version     string       `json:"version"`
	ReleaseType string       `json:"releaseType"`
	Channel     string       `json:"channel,omitempty"`
	ReleaseDate SnapshotDate `json:"releaseDate"`
	// PublicationDate is when the vendor announced it, which is not always when they
	// released it. Omitted entirely when unknown rather than defaulted to the release
	// date, which would invent a second fact out of the first.
	PublicationDate *SnapshotDate `json:"publicationDate,omitempty"`
	ReleaseNotesURL string        `json:"releaseNotesUrl,omitempty"`
	// Recommended is a pointer because "the vendor did not say" and "the vendor said
	// no" are different answers, and a snapshot that flattened them to false would
	// hand every consumer the wrong one.
	Recommended     *bool     `json:"recommended"`
	Withdrawn       bool      `json:"withdrawn"`
	WithdrawnReason string    `json:"withdrawnReason,omitempty"`
	FirstObservedAt time.Time `json:"firstObservedAt"`
	LastVerifiedAt  time.Time `json:"lastVerifiedAt"`
	PublishedAt     time.Time `json:"publishedAt"`
}

// SnapshotDate carries a date at exactly the precision the vendor published.
//
// Value is rendered by domain.PartialDate.String(), so it is "2026-09-04", "2026-09",
// "2026" or empty -- never a zero-filled day the vendor never stated. Precision is
// always present, including when Value is empty, so "we do not know" stays
// distinguishable from "we did not record it".
type SnapshotDate struct {
	Value     string `json:"value,omitempty"`
	Precision string `json:"precision"`
}

// SnapshotMapping is one product this release applies to.
type SnapshotMapping struct {
	Slug             string `json:"slug"`
	Channel          string `json:"channel,omitempty"`
	HardwareRevision string `json:"hardwareRevision,omitempty"`
	Region           string `json:"region,omitempty"`
	DeploymentMode   string `json:"deploymentMode,omitempty"`
	// IsLatestObserved is derived, not observed, and is exported anyway: rebuilding it
	// on import would mean ordering releases, and this catalogue does not order version
	// strings (ADR-0017). Carrying the flag preserves the answer the publishing use
	// case already computed from dates.
	IsLatestObserved bool `json:"isLatestObserved"`
}

// SnapshotEvidence is why the release above is believed.
type SnapshotEvidence struct {
	// SourceVendor and SourceSlug name the registry entry the value was read from.
	// Both may be empty for a fact whose source has since been retired from the
	// registry; the URL and hash still stand on their own.
	SourceVendor     string    `json:"sourceVendor,omitempty"`
	SourceSlug       string    `json:"sourceSlug,omitempty"`
	SourceURL        string    `json:"sourceUrl"`
	SourceType       string    `json:"sourceType,omitempty"`
	Official         bool      `json:"official"`
	RetrievedAt      time.Time `json:"retrievedAt"`
	ContentHash      string    `json:"contentHash,omitempty"`
	Excerpt          string    `json:"excerpt"`
	RawValue         string    `json:"rawValue,omitempty"`
	NormalizedValue  string    `json:"normalizedValue,omitempty"`
	CollectorID      string    `json:"collectorId,omitempty"`
	CollectorVersion string    `json:"collectorVersion,omitempty"`
	DiscoveryMethod  string    `json:"discoveryMethod"`
	// AIModelID and AIPromptVersion are exported whenever discovery was AI-assisted,
	// because a consumer deciding how much to trust a fact needs to know a model
	// produced it. The database CHECK requires both when the method says so.
	AIModelID       string   `json:"aiModelId,omitempty"`
	AIPromptVersion string   `json:"aiPromptVersion,omitempty"`
	Confidence      *float64 `json:"confidence,omitempty"`
}

// SnapshotManifest describes an export.
type SnapshotManifest struct {
	FormatVersion int       `json:"formatVersion"`
	GeneratedAt   time.Time `json:"generatedAt"`
	ReleaseCount  int       `json:"releaseCount"`
	// Vendors and Products are the slugs the records reference, sorted. They are a
	// reader's index and an importer's precondition: a snapshot naming a product the
	// local registry does not have cannot be imported, and saying so before writing
	// anything is better than a partial import.
	Vendors  []string `json:"vendors"`
	Products []string `json:"products"`
	License  string   `json:"license"`
	Notice   string   `json:"notice"`
}

// SnapshotLicense is the licence the exported data carries. Code is Apache-2.0; the
// data is not code, and CC BY 4.0 is chosen over the Open Data Commons licences because
// it addresses the EU sui generis database right explicitly. See ADR-0009.
const SnapshotLicense = "CC-BY-4.0"

// SnapshotNotice travels inside the manifest so that a copy of the file separated from
// this repository still says what it is and what it is not.
const SnapshotNotice = "FirmScout catalogue snapshot. Facts observed from the vendors' own " +
	"published sources, each with the evidence that justifies it. Verify against the " +
	"linked source before acting on a version: this is a record of what a vendor " +
	"published when FirmScout looked, not a guarantee of what is current now."

// SnapshotRepository reads and writes the observed facts a snapshot carries.
//
// It is one port rather than a method on ReleaseRepository because it is the only place
// that reads the catalogue whole. Every other read is scoped to a product and paged;
// giving that interface an unbounded "all of it" method would put a full-table scan one
// autocomplete away from a request handler.
type SnapshotRepository interface {
	// ExportReleaseFacts returns every published release with its evidence and product
	// mappings, ordered deterministically so that re-exporting an unchanged catalogue
	// produces a byte-identical file and Git records no change.
	ExportReleaseFacts(ctx context.Context) ([]ReleaseFact, error)
	// ImportReleaseFacts writes facts that are not already present, resolving vendor,
	// product and source slugs against the local registry. It returns how many were
	// written and how many were already there. A fact whose slugs do not resolve is an
	// error, never a silently skipped record.
	ImportReleaseFacts(ctx context.Context, facts []ReleaseFact) (imported, skipped int, err error)
}

// Validate checks a fact carries what a consumer needs to trust it.
//
// It runs on import as well as export, because a snapshot is a file anybody can edit:
// the importer must not assume the exporter wrote it.
func (f ReleaseFact) Validate() error {
	switch {
	case strings.TrimSpace(f.Release.ID) == "":
		return fmt.Errorf("release id is empty: %w", domain.ErrValidation)
	case strings.TrimSpace(f.Release.Version) == "":
		return fmt.Errorf("release %s has no version: %w", f.Release.ID, domain.ErrValidation)
	case strings.TrimSpace(f.Vendor) == "":
		return fmt.Errorf("release %s names no vendor: %w", f.Release.ID, domain.ErrValidation)
	case len(f.Products) == 0:
		// A release mapped to nothing is unreachable: no product page can show it and
		// no query can find it. Publication maintains that invariant, so a snapshot
		// violating it was edited by hand or exported from a corrupted catalogue.
		return fmt.Errorf("release %s maps to no product: %w", f.Release.ID, domain.ErrValidation)
	case strings.TrimSpace(f.Evidence.SourceURL) == "":
		return fmt.Errorf("release %s has evidence with no source URL: %w", f.Release.ID, domain.ErrValidation)
	case strings.TrimSpace(f.Evidence.Excerpt) == "":
		return fmt.Errorf("release %s has evidence with no excerpt: %w", f.Release.ID, domain.ErrValidation)
	}
	if !domain.ValidDatePrecision(domain.DatePrecision(f.Release.ReleaseDate.Precision)) {
		return fmt.Errorf("release %s has precision %q: %w",
			f.Release.ID, f.Release.ReleaseDate.Precision, domain.ErrValidation)
	}
	// The pairing is the whole date discipline in one check: a value without a
	// precision is a date nobody can render safely, and a precision claiming to know
	// something with no value behind it is worse.
	hasValue := strings.TrimSpace(f.Release.ReleaseDate.Value) != ""
	knownPrecision := domain.DatePrecision(f.Release.ReleaseDate.Precision) != domain.PrecisionUnknown
	if hasValue != knownPrecision {
		return fmt.Errorf("release %s has date %q at precision %q; a value and a known precision must accompany each other: %w",
			f.Release.ID, f.Release.ReleaseDate.Value, f.Release.ReleaseDate.Precision, domain.ErrValidation)
	}
	for _, p := range f.Products {
		if !domain.ValidSlug(p.Slug) {
			return fmt.Errorf("release %s maps to product %q, which is not a slug: %w",
				f.Release.ID, p.Slug, domain.ErrValidation)
		}
	}
	return nil
}
