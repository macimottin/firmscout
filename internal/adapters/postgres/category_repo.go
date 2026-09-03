package postgres

import (
	"context"
	"fmt"

	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/domain"
)

// CategoryRepo persists the product category vocabulary.
//
// The vocabulary is small and read constantly, but it is not cached here: caching a
// registry table inside a repository hides the moment a sync changes it, and the query
// is a single indexed lookup.
type CategoryRepo struct {
	db *DB
}

var _ application.CategoryRepository = (*CategoryRepo)(nil)

// NewCategoryRepo builds the repository.
func NewCategoryRepo(db *DB) *CategoryRepo { return &CategoryRepo{db: db} }

const categoryColumns = `id, slug, name, parent_id, description, managed_by, registry_path`

// GetBySlug returns one category, or domain.ErrNotFound.
func (r *CategoryRepo) GetBySlug(ctx context.Context, slug string) (domain.Category, error) {
	row := r.db.q(ctx).QueryRow(ctx,
		`SELECT `+categoryColumns+` FROM categories WHERE slug = $1`, slug)
	c, err := scanCategory(row)
	if err != nil {
		return domain.Category{}, wrap("category "+slug, err)
	}
	return c, nil
}

// List returns the whole vocabulary, ordered by slug so output is reproducible.
func (r *CategoryRepo) List(ctx context.Context) ([]domain.Category, error) {
	rows, err := r.db.q(ctx).Query(ctx,
		`SELECT `+categoryColumns+` FROM categories ORDER BY slug`)
	if err != nil {
		return nil, wrap("list categories", err)
	}
	defer rows.Close()

	var out []domain.Category
	for rows.Next() {
		c, err := scanCategory(rows)
		if err != nil {
			return nil, wrap("scan category", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("list categories", err)
	}
	return out, nil
}

// Upsert inserts or updates a category by slug.
func (r *CategoryRepo) Upsert(ctx context.Context, c domain.Category) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if c.ID == "" {
		c.ID = r.db.newID("cat")
	}
	if c.ManagedBy == "" {
		c.ManagedBy = domain.ManagedByRegistry
	}
	_, err := r.db.q(ctx).Exec(ctx,
		`INSERT INTO categories (id, slug, name, parent_id, description, managed_by, registry_path, created_at, updated_at)
         VALUES ($1, $2, $3, $4, $5, $6, $7, now(), now())
         ON CONFLICT (slug) DO UPDATE SET
            name          = EXCLUDED.name,
            parent_id     = EXCLUDED.parent_id,
            description   = EXCLUDED.description,
            managed_by    = EXCLUDED.managed_by,
            registry_path = EXCLUDED.registry_path,
            updated_at    = now()`,
		c.ID, c.Slug, c.Name, nullString(c.ParentID), nullString(c.Description),
		string(c.ManagedBy), nullString(c.RegistryPath))
	if err != nil {
		return wrap(fmt.Sprintf("upsert category %s", c.Slug), err)
	}
	return nil
}

// rowScanner is satisfied by both pgx.Row and pgx.Rows, so one scan function serves
// the single-row and multi-row paths.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanCategory(s rowScanner) (domain.Category, error) {
	var (
		c           domain.Category
		parent      *string
		description *string
		registry    *string
		managedBy   string
	)
	if err := s.Scan(&c.ID, &c.Slug, &c.Name, &parent, &description, &managedBy, &registry); err != nil {
		return domain.Category{}, err
	}
	c.ParentID = str(parent)
	c.Description = str(description)
	c.RegistryPath = str(registry)
	c.ManagedBy = domain.ManagedBy(managedBy)
	return c, nil
}
