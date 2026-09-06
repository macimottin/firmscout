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
	// Aliases and Relationships are COMPLETE sets, not additions.
	//
	// A document that carries a Product declares that product's entire alias set and
	// its entire outgoing relationship set, and an empty list means "none" rather than
	// "unspecified": SyncRegistry replaces both with exactly what is here, so removing
	// an `aliases:` or a `runs:` block retracts what it used to assert. A document that
	// carries no Product declares neither, and may not carry a relationship at all --
	// there is nothing for the edge to hang off, and the sync rejects it by name.
	//
	// That is the whole of the "does this document manage relationships?" question, and
	// it is answered by Product being non-nil rather than by a flag or by the
	// difference between a nil and an empty slice. Only a product document has ever had
	// a `runs:` or an `aliases:` key to write, so a third state -- "a product document
	// that opts out of managing its own edges" -- would be a state no file can reach,
	// and encoding it in Go's nil-versus-empty distinction would put a retraction that
	// deletes database rows behind a difference no YAML author can see, no round trip
	// through a serialiser preserves, and no reviewer can spot in a diff.
	Aliases       []domain.ProductAlias
	Relationships []domain.ProductRelationship
	Source        *domain.Source
	// SourceProducts lists the product slugs a source covers, which is how one
	// catalogue fetch comes to serve many products.
	//
	// Parsed, and not yet applied. SyncRegistry does not write the source_products
	// rows this field describes, so a contributor who fills `covers_products:` in a
	// dataset YAML today gets no effect and no warning, and
	// SourceRepository.ProductsForSource answers empty for every source in a synced
	// database. The table, the port method and the adapter query all exist; only the
	// write is missing. It is recorded here rather than removed because the field is
	// already in packages/schemas/source.schema.json and in authored files, and a
	// reader of this struct is exactly who needs to know the difference between
	// "loaded" and "has an effect".
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
	summaries  SummaryRefresher
}

// WithSummaries attaches the refresher that makes upserted products readable.
//
// It is a builder method rather than an eighth constructor parameter so that adding it
// does not break every existing call site at once -- the same concession
// NewSyncRegistry's doc comment already makes for a nil categories repository. Without
// it the sync still upserts everything and reports a warning naming the consequence,
// because a catalogue that is silently unreadable is worse than one that says so.
func (uc *SyncRegistry) WithSummaries(s SummaryRefresher) *SyncRegistry {
	uc.summaries = s
	return uc
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
	// RelationshipsUpserted counts the product-to-product edges written, which for
	// now is one runs_os edge per hardware model: the link that takes a fleet
	// manager from a model number to the operating system whose releases they want.
	RelationshipsUpserted int
	// SummariesRefreshed counts the products made readable by this sync. A product with
	// no product_summaries row is a 404 on the public API and absent from search, so a
	// sync that upserts products and refreshes nothing has produced an invisible
	// catalogue -- which is why this is reported rather than assumed.
	SummariesRefreshed int
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
	//
	// upsertedProductIDs is kept alongside productIDBySlug because the two answer
	// different questions. The map accumulates every product this run has resolved an
	// id for, including ones it only looked up to satisfy a source or a relationship;
	// the slice is exactly what this run wrote, which is what the summary pass below
	// must refresh and no more.
	productIDBySlug := map[string]string{}
	var upsertedProductIDs []string
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
		upsertedProductIDs = append(upsertedProductIDs, p.ID)
		report.ProductsUpserted++

		// Unconditionally, including for the empty set: see RegistryDocument.Aliases.
		// Guarding this on len(d.Aliases) > 0 is what made the replace unable to
		// replace with nothing, so deleting the last alias from a file left it
		// asserted in the database and searchable forever.
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

	// Relationships after every product exists, never during the product pass: a
	// device document names the operating system it runs by slug, and the file
	// declaring that operating system may be read after the device's own. Resolving
	// the target here means file order cannot decide whether a sync succeeds.
	//
	// Every product document is replaced, including one declaring no edge at all. The
	// pass used to skip a document with an empty list, which made ReplaceRelationships
	// a port that could not replace with nothing: deleting a `runs:` block left the
	// edge in product_relationships and on the device page, still telling an operator
	// that this chassis runs an operating system the reviewed file has since stopped
	// claiming it runs. Retraction is the reason a replace is a replace.
	for _, d := range docs {
		if d.Product == nil {
			// A document with no product has nowhere to hang an edge. Rejecting it
			// names the file; silently skipping it would drop a fact a contributor
			// wrote down.
			if len(d.Relationships) > 0 {
				return report, fmt.Errorf("%s: declares relationships without a product: %w",
					d.Path, domain.ErrValidation)
			}
			continue
		}
		fromID, err := resolveProduct(d.Path, d.Product.Slug)
		if err != nil {
			return report, err
		}
		rels := make([]domain.ProductRelationship, 0, len(d.Relationships))
		for _, r := range d.Relationships {
			// ToProductID still holds the target's registry slug at this point; the
			// loader cannot resolve it, because it reads one file at a time.
			toID, err := resolveProduct(d.Path, r.ToProductID)
			if err != nil {
				return report, err
			}
			r.FromProductID = fromID
			r.ToProductID = toID
			if r.ID == "" {
				r.ID = uc.ids.NewID("prl")
			}
			r.ManagedBy = domain.ManagedByRegistry
			r.RegistryPath = d.Path
			if err := r.Validate(); err != nil {
				return report, fmt.Errorf("%s: %w", d.Path, err)
			}
			rels = append(rels, r)
		}
		if err := uc.products.ReplaceRelationships(ctx, fromID, rels); err != nil {
			return report, fmt.Errorf("%s: replace relationships: %w", d.Path, err)
		}
		report.RelationshipsUpserted += len(rels)
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

	// Summaries last of all, because a product with no product_summaries row is a 404
	// on the public API and absent from search -- an upsert that stops here has
	// registered a catalogue nobody can read. The ids are walked in sorted order so
	// two runs over the same registry do the same work in the same sequence.
	if uc.summaries == nil {
		if len(upsertedProductIDs) > 0 {
			report.Warnings = append(report.Warnings, fmt.Sprintf(
				"product summaries were not refreshed: %d product(s) will not be served by the public API until a refresh runs",
				len(upsertedProductIDs)))
		}
	} else {
		ids := append([]string(nil), upsertedProductIDs...)
		sort.Strings(ids)
		for _, id := range ids {
			if err := uc.summaries.RefreshProductSummary(ctx, id); err != nil {
				return report, fmt.Errorf("refresh summary for product %s: %w", id, err)
			}
			report.SummariesRefreshed++
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
