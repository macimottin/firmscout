package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/domain"
)

// ProductRepo is the PostgreSQL implementation of application.ProductRepository. It
// owns three tables that always move together: products, product_aliases and the
// product_categories join.
type ProductRepo struct{ db *DB }

var _ application.ProductRepository = (*ProductRepo)(nil)

// NewProductRepo returns a product repository bound to db.
func NewProductRepo(db *DB) *ProductRepo { return &ProductRepo{db: db} }

// productSelect reads a product together with its category slugs. The slugs are
// gathered in a correlated subquery rather than a join so that a product with three
// categories is still one row; a join here would silently multiply the result set of
// every list query.
const productSelect = `
SELECT p.id, p.vendor_id, p.product_family_id, p.slug, p.name, p.model_identifier,
       p.description, p.default_release_type, p.lifecycle_status, p.popularity_score,
       p.security_critical, p.managed_by, p.registry_path, p.created_at, p.updated_at,
       COALESCE((SELECT array_agg(c.slug ORDER BY c.slug)
                   FROM product_categories pc
                   JOIN categories c ON c.id = pc.category_id
                  WHERE pc.product_id = p.id), '{}'::text[]) AS category_slugs
  FROM products p`

func scanProduct(row interface{ Scan(...any) error }) (domain.Product, error) {
	var (
		p            domain.Product
		familyID     *string
		modelID      *string
		description  *string
		releaseType  *string
		registryPath *string
		managedBy    string
		lifecycle    string
		categories   []string
	)
	if err := row.Scan(
		&p.ID, &p.VendorID, &familyID, &p.Slug, &p.Name, &modelID, &description,
		&releaseType, &lifecycle, &p.PopularityScore, &p.SecurityCritical,
		&managedBy, &registryPath, &p.CreatedAt, &p.UpdatedAt, &categories,
	); err != nil {
		return domain.Product{}, err
	}
	p.ProductFamilyID = str(familyID)
	p.ModelIdentifier = str(modelID)
	p.Description = str(description)
	p.DefaultReleaseType = domain.ReleaseType(str(releaseType))
	p.LifecycleStatus = domain.LifecycleStatus(lifecycle)
	p.ManagedBy = domain.ManagedBy(managedBy)
	p.RegistryPath = str(registryPath)
	p.CategorySlugs = categories
	return p, nil
}

// GetByID returns the product with the given id, or domain.ErrNotFound.
func (r *ProductRepo) GetByID(ctx context.Context, id string) (domain.Product, error) {
	row := r.db.q(ctx).QueryRow(ctx, productSelect+` WHERE p.id = $1`, id)
	p, err := scanProduct(row)
	if err != nil {
		return domain.Product{}, wrap("product.GetByID", err)
	}
	return p, nil
}

// GetBySlug returns the product with the given slug, or domain.ErrNotFound. Product
// slugs are globally unique, not per-vendor, because they appear in public URLs.
func (r *ProductRepo) GetBySlug(ctx context.Context, slug string) (domain.Product, error) {
	row := r.db.q(ctx).QueryRow(ctx, productSelect+` WHERE p.slug = $1`, slug)
	p, err := scanProduct(row)
	if err != nil {
		return domain.Product{}, wrap("product.GetBySlug", err)
	}
	return p, nil
}

// ListByVendor returns a page of a vendor's products ordered by slug, with the cursor
// for the next page. An empty returned cursor means there are no more rows.
func (r *ProductRepo) ListByVendor(ctx context.Context, vendorID string, limit int, cursor string) ([]domain.Product, string, error) {
	limit = clampLimit(limit, 50, 500)
	parts, err := decodeCursor(cursor, 1)
	if err != nil {
		return nil, "", err
	}
	after := ""
	if parts != nil {
		after = parts[0]
	}

	rows, err := r.db.q(ctx).Query(ctx,
		productSelect+`
         WHERE p.vendor_id = $1 AND ($2::text = '' OR p.slug > $2)
         ORDER BY p.slug
         LIMIT $3`, vendorID, after, limit)
	if err != nil {
		return nil, "", wrap("product.ListByVendor", err)
	}
	defer rows.Close()

	out := make([]domain.Product, 0, limit)
	for rows.Next() {
		p, err := scanProduct(rows)
		if err != nil {
			return nil, "", wrap("product.ListByVendor.scan", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, "", wrap("product.ListByVendor.rows", err)
	}

	next := ""
	if len(out) == limit {
		next = encodeCursor(out[len(out)-1].Slug)
	}
	return out, next, nil
}

// Upsert inserts or updates a product together with its category memberships.
//
// The product row and its categories are written in one transaction, so a product can
// never be visible with a half-applied category set.
//
// A category slug that has no row in the categories table is an error rather than a
// silently dropped value. Categories are registry-managed and synchronised before
// products; a slug that is missing means the registry sync ran out of order or the
// slug is a typo, and both are worth failing on rather than discovering later as a
// product that mysteriously appears in no category.
func (r *ProductRepo) Upsert(ctx context.Context, p domain.Product) error {
	if err := p.Validate(); err != nil {
		return err
	}
	managedBy := p.ManagedBy
	if managedBy == "" {
		managedBy = domain.ManagedByRegistry
	}
	releaseType := ""
	if p.DefaultReleaseType != "" {
		releaseType = string(p.DefaultReleaseType)
	}

	return r.db.Within(ctx, func(ctx context.Context) error {
		q := r.db.q(ctx)

		if _, err := q.Exec(ctx,
			`INSERT INTO products (
                id, vendor_id, product_family_id, slug, name, model_identifier, description,
                default_release_type, lifecycle_status, popularity_score, security_critical,
                managed_by, registry_path, created_at, updated_at
             ) VALUES (
                $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13,
                COALESCE($14::timestamptz, now()), COALESCE($15::timestamptz, now())
             )
             ON CONFLICT (id) DO UPDATE SET
                vendor_id            = EXCLUDED.vendor_id,
                product_family_id    = EXCLUDED.product_family_id,
                slug                 = EXCLUDED.slug,
                name                 = EXCLUDED.name,
                model_identifier     = EXCLUDED.model_identifier,
                description          = EXCLUDED.description,
                default_release_type = EXCLUDED.default_release_type,
                lifecycle_status     = EXCLUDED.lifecycle_status,
                popularity_score     = EXCLUDED.popularity_score,
                security_critical    = EXCLUDED.security_critical,
                managed_by           = EXCLUDED.managed_by,
                registry_path        = EXCLUDED.registry_path,
                updated_at           = now()`,
			p.ID, p.VendorID, nullString(p.ProductFamilyID), p.Slug, p.Name,
			nullString(p.ModelIdentifier), nullString(p.Description),
			nullString(releaseType), string(p.LifecycleStatus), p.PopularityScore,
			p.SecurityCritical, string(managedBy), nullString(p.RegistryPath),
			nullTime(p.CreatedAt), nullTime(p.UpdatedAt),
		); err != nil {
			return wrap("product.Upsert", err)
		}

		return r.replaceCategories(ctx, p.ID, p.CategorySlugs)
	})
}

// replaceCategories makes product_categories match slugs exactly.
func (r *ProductRepo) replaceCategories(ctx context.Context, productID string, slugs []string) error {
	q := r.db.q(ctx)
	if slugs == nil {
		slugs = []string{}
	}

	if len(slugs) > 0 {
		rows, err := q.Query(ctx,
			`SELECT t.s FROM unnest($1::text[]) AS t (s)
              WHERE NOT EXISTS (SELECT 1 FROM categories c WHERE c.slug = t.s)`, slugs)
		if err != nil {
			return wrap("product.Upsert.categories.check", err)
		}
		var missing []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				rows.Close()
				return wrap("product.Upsert.categories.scan", err)
			}
			missing = append(missing, s)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return wrap("product.Upsert.categories.rows", err)
		}
		if len(missing) > 0 {
			return fmt.Errorf("product.Upsert: unknown category slugs %v: %w", missing, domain.ErrValidation)
		}
	}

	if _, err := q.Exec(ctx,
		`DELETE FROM product_categories
          WHERE product_id = $1
            AND category_id NOT IN (SELECT id FROM categories WHERE slug = ANY($2::text[]))`,
		productID, slugs); err != nil {
		return wrap("product.Upsert.categories.delete", err)
	}

	if len(slugs) == 0 {
		return nil
	}
	_, err := q.Exec(ctx,
		`INSERT INTO product_categories (product_id, category_id)
         SELECT $1, c.id FROM categories c WHERE c.slug = ANY($2::text[])
         ON CONFLICT DO NOTHING`, productID, slugs)
	return wrap("product.Upsert.categories.insert", err)
}

// ---------------------------------------------------------------------------
// Families
// ---------------------------------------------------------------------------

const familyColumns = `
    id, vendor_id, slug, name, description, managed_by, registry_path`

func scanFamily(row interface{ Scan(...any) error }) (domain.ProductFamily, error) {
	var (
		f            domain.ProductFamily
		description  *string
		registryPath *string
		managedBy    string
	)
	if err := row.Scan(&f.ID, &f.VendorID, &f.Slug, &f.Name, &description, &managedBy, &registryPath); err != nil {
		return domain.ProductFamily{}, err
	}
	f.Description = str(description)
	f.ManagedBy = domain.ManagedBy(managedBy)
	f.RegistryPath = str(registryPath)
	return f, nil
}

// UpsertFamily inserts or updates a product family. Families are what let one firmware
// image serve forty switch models, so the (vendor, slug) pair is unique and a second
// family claiming the same pair is reported as domain.ErrConflict.
func (r *ProductRepo) UpsertFamily(ctx context.Context, f domain.ProductFamily) error {
	if err := f.Validate(); err != nil {
		return err
	}
	managedBy := f.ManagedBy
	if managedBy == "" {
		managedBy = domain.ManagedByRegistry
	}
	_, err := r.db.q(ctx).Exec(ctx,
		`INSERT INTO product_families (
            id, vendor_id, slug, name, description, managed_by, registry_path
         ) VALUES ($1, $2, $3, $4, $5, $6, $7)
         ON CONFLICT (id) DO UPDATE SET
            vendor_id     = EXCLUDED.vendor_id,
            slug          = EXCLUDED.slug,
            name          = EXCLUDED.name,
            description   = EXCLUDED.description,
            managed_by    = EXCLUDED.managed_by,
            registry_path = EXCLUDED.registry_path,
            updated_at    = now()`,
		f.ID, f.VendorID, f.Slug, f.Name, nullString(f.Description),
		string(managedBy), nullString(f.RegistryPath))
	return wrap("product.UpsertFamily", err)
}

// GetFamilyBySlug returns a vendor's family by slug, or domain.ErrNotFound. Family
// slugs are unique per vendor, not globally, so the vendor id is part of the lookup.
func (r *ProductRepo) GetFamilyBySlug(ctx context.Context, vendorID, slug string) (domain.ProductFamily, error) {
	row := r.db.q(ctx).QueryRow(ctx,
		`SELECT`+familyColumns+` FROM product_families WHERE vendor_id = $1 AND slug = $2`,
		vendorID, slug)
	f, err := scanFamily(row)
	if err != nil {
		return domain.ProductFamily{}, wrap("product.GetFamilyBySlug", err)
	}
	return f, nil
}

// ---------------------------------------------------------------------------
// Aliases
// ---------------------------------------------------------------------------

// ReplaceAliases makes a product's alias set exactly the given list, in one
// transaction.
//
// Replacement rather than merge is deliberate: the registry file is the source of
// truth for aliases, and an alias removed from that file must disappear rather than
// linger and keep matching candidates to a product it no longer names.
func (r *ProductRepo) ReplaceAliases(ctx context.Context, productID string, aliases []domain.ProductAlias) error {
	return r.db.Within(ctx, func(ctx context.Context) error {
		q := r.db.q(ctx)
		if _, err := q.Exec(ctx, `DELETE FROM product_aliases WHERE product_id = $1`, productID); err != nil {
			return wrap("product.ReplaceAliases.delete", err)
		}
		for _, a := range aliases {
			id := a.ID
			if id == "" {
				id = r.db.newID("pal")
			}
			kind := a.Kind
			if kind == "" {
				kind = domain.AliasMarketingName
			}
			normalized := a.NormalizedAlias
			if normalized == "" {
				normalized = domain.NormalizeAlias(a.Alias)
			}
			if _, err := q.Exec(ctx,
				`INSERT INTO product_aliases (
                    id, product_id, alias, normalized_alias, alias_kind, source_note
                 ) VALUES ($1, $2, $3, $4, $5, $6)`,
				id, productID, a.Alias, normalized, string(kind), nullString(a.SourceNote)); err != nil {
				return wrap("product.ReplaceAliases.insert", err)
			}
		}
		return nil
	})
}

// ListAliases returns a product's aliases ordered by their normalised form, which
// makes the output stable for diffing against a registry file.
func (r *ProductRepo) ListAliases(ctx context.Context, productID string) ([]domain.ProductAlias, error) {
	rows, err := r.db.q(ctx).Query(ctx,
		`SELECT id, product_id, alias, normalized_alias, alias_kind, source_note
           FROM product_aliases WHERE product_id = $1 ORDER BY normalized_alias`, productID)
	if err != nil {
		return nil, wrap("product.ListAliases", err)
	}
	defer rows.Close()

	var out []domain.ProductAlias
	for rows.Next() {
		var (
			a          domain.ProductAlias
			kind       string
			sourceNote *string
		)
		if err := rows.Scan(&a.ID, &a.ProductID, &a.Alias, &a.NormalizedAlias, &kind, &sourceNote); err != nil {
			return nil, wrap("product.ListAliases.scan", err)
		}
		a.Kind = domain.AliasKind(kind)
		a.SourceNote = str(sourceNote)
		out = append(out, a)
	}
	return out, wrap("product.ListAliases.rows", rows.Err())
}

// ResolveByAlias returns every product of the vendor whose slug or normalised alias
// matches, ordered by slug.
//
// The query deliberately has no LIMIT. Returning more than one row is the mechanism by
// which the caller detects ambiguity -- "Catalyst 9300" naming four products is a fact
// the ingestion pipeline must route to a human, and a LIMIT 1 would convert that fact
// into a silent, arbitrary and probably wrong choice.
//
// The alias match uses EXISTS rather than a join so that a product carrying two
// aliases which both match still counts as one product. A join would return it twice
// and manufacture ambiguity out of a unique match.
func (r *ProductRepo) ResolveByAlias(ctx context.Context, vendorID, hint string) ([]domain.Product, error) {
	// Two comparisons, because a hint arrives in whichever shape the source wrote it.
	// The slug is matched verbatim; aliases are matched in their normalised form, the
	// same transformation applied when they were stored. Comparing a normalised hint
	// against a raw slug -- "mikrotik routeros" against "mikrotik-routeros" -- silently
	// matches nothing, which reads downstream as "no such product".
	normalized := domain.NormalizeAlias(hint)
	rows, err := r.db.q(ctx).Query(ctx,
		productSelect+`
         WHERE p.vendor_id = $1
           AND (p.slug = $2
                OR EXISTS (SELECT 1 FROM product_aliases a
                            WHERE a.product_id = p.id AND a.normalized_alias = $3))
         ORDER BY p.slug`, vendorID, strings.TrimSpace(hint), normalized)
	if err != nil {
		return nil, wrap("product.ResolveByAlias", err)
	}
	defer rows.Close()

	var out []domain.Product
	for rows.Next() {
		p, err := scanProduct(rows)
		if err != nil {
			return nil, wrap("product.ResolveByAlias.scan", err)
		}
		out = append(out, p)
	}
	return out, wrap("product.ResolveByAlias.rows", rows.Err())
}
