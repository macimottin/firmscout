package application

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/macimottin/firmscout/internal/domain"
)

// RegistryDocument is one parsed registry file: a vendor, a product or a source.
//
// The registry is the curated half of FirmScout's hybrid dataset. It lives in Git so
// that adding a vendor is a pull request rather than a database grant, and it is
// synchronised into PostgreSQL by this use case. See ADR-0016.
type RegistryDocument struct {
	// Path is the file the document came from, retained so a validation error names
	// something a contributor can open.
	Path string

	Vendor   *domain.Vendor
	Category *domain.Category
	Family   *domain.ProductFamily
	Product  *domain.Product
	Aliases  []domain.ProductAlias
	Source   *domain.Source
	// SourceProducts lists the product slugs a source covers, which is how one
	// catalogue fetch comes to serve many products.
	SourceProducts []string
}

// RegistryLoader reads registry documents from somewhere: a directory of YAML on disk
// in the MVP, and potentially an object store or a Git ref later. The port exists so
// the sync use case never learns what YAML is.
type RegistryLoader interface {
	Load(ctx context.Context) ([]RegistryDocument, error)
}

// SyncRegistry applies the Git-managed registry to the database.
//
// The sync is deliberately additive and idempotent: it upserts what the files
// describe and leaves everything else alone. It does not delete rows that vanished
// from the registry, because a deletion in a catalogue with published releases
// pointing at it is a decision a human should make explicitly rather than a side
// effect of a file being moved.
type SyncRegistry struct {
	loader     RegistryLoader
	vendors    VendorRepository
	categories CategoryRepository
	products   ProductRepository
	sources    SourceRepository
	ids        IDGenerator
	clock      Clock
}

// NewSyncRegistry builds the use case.
//
// categories may be nil, in which case category references on products are not
// resolved. That is a concession to callers that only need vendors and sources; the
// CLI always supplies one.
func NewSyncRegistry(l RegistryLoader, v VendorRepository, cat CategoryRepository, p ProductRepository, s SourceRepository, ids IDGenerator, c Clock) *SyncRegistry {
	return &SyncRegistry{loader: l, vendors: v, categories: cat, products: p, sources: s, ids: ids, clock: c}
}

// SyncReport summarises what the sync did, so a CLI run prints something a maintainer
// can check rather than a silent success.
type SyncReport struct {
	VendorsUpserted    int
	CategoriesUpserted int
	FamiliesUpserted   int
	ProductsUpserted   int
	AliasesUpserted    int
	SourcesUpserted    int
	// SourcesLeftDisabled counts sources the registry declares but which remain
	// uncollected because their compliance status forbids it. This is reported
	// prominently rather than buried: a contributor who adds a source and sees
	// nothing happen should be told why.
	SourcesLeftDisabled int
	Warnings            []string
}

// Execute reads the registry and upserts it.
//
// Ordering matters and is enforced here rather than left to file iteration order:
// vendors, then families, then products and their aliases, then sources. A source
// referencing a product that does not exist yet is a validation error a contributor
// can act on, not a foreign-key violation from the driver.
func (uc *SyncRegistry) Execute(ctx context.Context) (SyncReport, error) {
	docs, err := uc.loader.Load(ctx)
	if err != nil {
		return SyncReport{}, fmt.Errorf("load registry: %w", err)
	}

	var report SyncReport
	now := uc.clock.Now()

	// Vendors first: everything else references one.
	vendorIDBySlug := map[string]string{}
	for _, d := range docs {
		if d.Vendor == nil {
			continue
		}
		v := *d.Vendor
		if err := v.Validate(); err != nil {
			return report, fmt.Errorf("%s: %w", d.Path, err)
		}
		existing, err := uc.vendors.GetBySlug(ctx, v.Slug)
		switch {
		case err == nil:
			v.ID = existing.ID
			v.CreatedAt = existing.CreatedAt
		case isNotFound(err):
			v.ID = uc.ids.NewID("ven")
			v.CreatedAt = now
		default:
			return report, fmt.Errorf("%s: look up vendor: %w", d.Path, err)
		}
		v.UpdatedAt = now
		v.ManagedBy = domain.ManagedByRegistry
		v.RegistryPath = d.Path
		if err := uc.vendors.Upsert(ctx, v); err != nil {
			return report, fmt.Errorf("%s: upsert vendor: %w", d.Path, err)
		}
		vendorIDBySlug[v.Slug] = v.ID
		report.VendorsUpserted++
	}

	resolveVendor := func(path, slug string) (string, error) {
		if id, ok := vendorIDBySlug[slug]; ok {
			return id, nil
		}
		v, err := uc.vendors.GetBySlug(ctx, slug)
		if err != nil {
			if isNotFound(err) {
				return "", fmt.Errorf("%s: references vendor %q, which is not in the registry: %w",
					path, slug, domain.ErrValidation)
			}
			return "", fmt.Errorf("%s: look up vendor %q: %w", path, slug, err)
		}
		vendorIDBySlug[slug] = v.ID
		return v.ID, nil
	}

	// Categories before products, because a product may reference one. A missing
	// category is a validation error naming the file, not a silently-created row.
	if uc.categories != nil {
		for _, d := range docs {
			if d.Category == nil {
				continue
			}
			cat := *d.Category
			if err := cat.Validate(); err != nil {
				return report, fmt.Errorf("%s: %w", d.Path, err)
			}
			// The document carries the parent's SLUG. It is cleared here and
			// resolved to an id in the second pass below, so a file may declare a
			// child before its parent without the file order mattering.
			parentSlug := cat.ParentID
			cat.ParentID = ""
			_ = parentSlug
			existing, err := uc.categories.GetBySlug(ctx, cat.Slug)
			switch {
			case err == nil:
				cat.ID = existing.ID
			case isNotFound(err):
				cat.ID = uc.ids.NewID("cat")
			default:
				return report, fmt.Errorf("%s: look up category: %w", d.Path, err)
			}
			cat.ManagedBy = domain.ManagedByRegistry
			cat.RegistryPath = d.Path
			if err := uc.categories.Upsert(ctx, cat); err != nil {
				return report, fmt.Errorf("%s: upsert category: %w", d.Path, err)
			}
			report.CategoriesUpserted++
		}

		// Parent links are applied in a second pass so a file may declare a child
		// before its parent without the order of the file list mattering.
		for _, d := range docs {
			if d.Category == nil || d.Category.ParentID == "" {
				continue
			}
			cat, err := uc.categories.GetBySlug(ctx, d.Category.Slug)
			if err != nil {
				return report, fmt.Errorf("%s: reload category: %w", d.Path, err)
			}
			if cat.Slug == d.Category.ParentID {
				return report, fmt.Errorf("%s: category %q names itself as its parent: %w",
					d.Path, cat.Slug, domain.ErrValidation)
			}
			parent, err := uc.categories.GetBySlug(ctx, d.Category.ParentID)
			if err != nil {
				if isNotFound(err) {
					return report, fmt.Errorf("%s: category %q names parent %q, which is not in the registry: %w",
						d.Path, cat.Slug, d.Category.ParentID, domain.ErrValidation)
				}
				return report, fmt.Errorf("%s: look up parent category: %w", d.Path, err)
			}
			if cat.ParentID == parent.ID {
				continue
			}
			cat.ParentID = parent.ID
			if err := uc.categories.Upsert(ctx, cat); err != nil {
				return report, fmt.Errorf("%s: link category parent: %w", d.Path, err)
			}
		}
	}

	// Families next.
	for _, d := range docs {
		if d.Family == nil {
			continue
		}
		f := *d.Family
		vendorID, err := resolveVendor(d.Path, f.VendorID)
		if err != nil {
			return report, err
		}
		f.VendorID = vendorID
		if err := f.Validate(); err != nil {
			return report, fmt.Errorf("%s: %w", d.Path, err)
		}
		existing, err := uc.products.GetFamilyBySlug(ctx, vendorID, f.Slug)
		switch {
		case err == nil:
			f.ID = existing.ID
		case isNotFound(err):
			f.ID = uc.ids.NewID("fam")
		default:
			return report, fmt.Errorf("%s: look up family: %w", d.Path, err)
		}
		f.ManagedBy = domain.ManagedByRegistry
		f.RegistryPath = d.Path
		if err := uc.products.UpsertFamily(ctx, f); err != nil {
			return report, fmt.Errorf("%s: upsert family: %w", d.Path, err)
		}
		report.FamiliesUpserted++
	}

	// Products and their aliases.
	productIDBySlug := map[string]string{}
	for _, d := range docs {
		if d.Product == nil {
			continue
		}
		p := *d.Product
		vendorID, err := resolveVendor(d.Path, p.VendorID)
		if err != nil {
			return report, err
		}
		p.VendorID = vendorID

		if p.ProductFamilyID != "" {
			fam, err := uc.products.GetFamilyBySlug(ctx, vendorID, p.ProductFamilyID)
			if err != nil {
				if isNotFound(err) {
					return report, fmt.Errorf("%s: references family %q, which is not in the registry: %w",
						d.Path, p.ProductFamilyID, domain.ErrValidation)
				}
				return report, fmt.Errorf("%s: look up family: %w", d.Path, err)
			}
			p.ProductFamilyID = fam.ID
		}

		if err := p.Validate(); err != nil {
			return report, fmt.Errorf("%s: %w", d.Path, err)
		}
		existing, err := uc.products.GetBySlug(ctx, p.Slug)
		switch {
		case err == nil:
			p.ID = existing.ID
			p.CreatedAt = existing.CreatedAt
		case isNotFound(err):
			p.ID = uc.ids.NewID("prd")
			p.CreatedAt = now
		default:
			return report, fmt.Errorf("%s: look up product: %w", d.Path, err)
		}
		p.UpdatedAt = now
		p.ManagedBy = domain.ManagedByRegistry
		p.RegistryPath = d.Path
		if err := uc.products.Upsert(ctx, p); err != nil {
			return report, fmt.Errorf("%s: upsert product: %w", d.Path, err)
		}
		productIDBySlug[p.Slug] = p.ID
		report.ProductsUpserted++

		if len(d.Aliases) > 0 {
			aliases := make([]domain.ProductAlias, 0, len(d.Aliases))
			seen := map[string]bool{}
			for _, a := range d.Aliases {
				a.ProductID = p.ID
				if a.ID == "" {
					a.ID = uc.ids.NewID("ali")
				}
				if seen[a.NormalizedAlias] {
					report.Warnings = append(report.Warnings,
						fmt.Sprintf("%s: duplicate alias %q ignored", d.Path, a.Alias))
					continue
				}
				seen[a.NormalizedAlias] = true
				aliases = append(aliases, a)
			}
			if err := uc.products.ReplaceAliases(ctx, p.ID, aliases); err != nil {
				return report, fmt.Errorf("%s: replace aliases: %w", d.Path, err)
			}
			report.AliasesUpserted += len(aliases)
		}
	}

	resolveProduct := func(path, slug string) (string, error) {
		if id, ok := productIDBySlug[slug]; ok {
			return id, nil
		}
		p, err := uc.products.GetBySlug(ctx, slug)
		if err != nil {
			if isNotFound(err) {
				return "", fmt.Errorf("%s: references product %q, which is not in the registry: %w",
					path, slug, domain.ErrValidation)
			}
			return "", fmt.Errorf("%s: look up product %q: %w", path, slug, err)
		}
		productIDBySlug[slug] = p.ID
		return p.ID, nil
	}

	// Sources last.
	for _, d := range docs {
		if d.Source == nil {
			continue
		}
		s := *d.Source
		vendorID, err := resolveVendor(d.Path, s.VendorID)
		if err != nil {
			return report, err
		}
		s.VendorID = vendorID

		if s.ProductID != "" {
			id, err := resolveProduct(d.Path, s.ProductID)
			if err != nil {
				return report, err
			}
			s.ProductID = id
		}

		if err := s.Validate(); err != nil {
			return report, fmt.Errorf("%s: %w", d.Path, err)
		}

		existing, err := uc.sources.GetBySlug(ctx, vendorID, s.Slug)
		switch {
		case err == nil:
			s.ID = existing.ID
			// Change-detection state belongs to the running system, not to the
			// registry. Overwriting it on every sync would make every source look
			// changed and re-run every collector.
			s.ETag = existing.ETag
			s.LastModifiedValue = existing.LastModifiedValue
			s.NormalizedContentHash = existing.NormalizedContentHash
			s.LastCheckedAt = existing.LastCheckedAt
			s.LastChangedAt = existing.LastChangedAt
			s.LastSuccessAt = existing.LastSuccessAt
			s.NextCheckAt = existing.NextCheckAt
			s.ConsecutiveFailures = existing.ConsecutiveFailures
			s.ConsecutiveUnchanged = existing.ConsecutiveUnchanged
			s.RetryAfterUntil = existing.RetryAfterUntil
			if existing.Health != domain.SourceDiscovered {
				s.Health = existing.Health
			}
		case isNotFound(err):
			s.ID = uc.ids.NewID("src")
			if s.NextCheckAt.IsZero() {
				s.NextCheckAt = now
			}
			if s.Health == "" {
				s.Health = domain.SourceDiscovered
			}
		default:
			return report, fmt.Errorf("%s: look up source: %w", d.Path, err)
		}

		s.ManagedBy = domain.ManagedByRegistry
		s.RegistryPath = d.Path
		if err := uc.sources.Upsert(ctx, s); err != nil {
			return report, fmt.Errorf("%s: upsert source: %w", d.Path, err)
		}
		report.SourcesUpserted++

		if !s.Enabled || !s.CompliancePermitsCollection() {
			report.SourcesLeftDisabled++
			report.Warnings = append(report.Warnings, describeUncollectable(d.Path, s))
		}
	}

	sort.Strings(report.Warnings)
	return report, nil
}

// describeUncollectable explains, in one sentence a contributor can act on, why a
// registered source will not be checked.
func describeUncollectable(path string, s domain.Source) string {
	var reasons []string
	if !s.Enabled {
		reasons = append(reasons, "it is not enabled")
	}
	switch s.RobotsPolicyStatus {
	case domain.RobotsDisallowed:
		reasons = append(reasons, "the host's robots.txt disallows this path")
	case domain.RobotsUnknown:
		reasons = append(reasons, "its robots policy has not been determined")
	}
	switch s.TermsReviewStatus {
	case domain.TermsPending:
		reasons = append(reasons, "its terms of use have not been reviewed")
	case domain.TermsProhibited:
		reasons = append(reasons, "its terms of use prohibit collection")
	}
	if len(reasons) == 0 {
		reasons = append(reasons, "its compliance status does not permit collection")
	}
	return fmt.Sprintf("%s: source %q will not be checked because %s",
		path, s.Slug, strings.Join(reasons, ", and "))
}
