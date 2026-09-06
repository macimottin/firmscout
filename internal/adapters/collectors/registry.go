package collectors

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strings"

	"github.com/macimottin/firmscout/collectors/sdk"
	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/domain"
)

// Registry resolves a source to the collector that handles it.
//
// It implements application.CollectorRegistry. Every collector it holds is built from
// a config at construction time, so a malformed config is a startup failure rather
// than a mystery at the first scheduled check of a source nobody looks at.
type Registry struct {
	byID  map[string]application.Collector
	order []string
}

// RegistryOption configures registry construction.
type RegistryOption func(*registryOptions)

type registryOptions struct {
	logger *slog.Logger
	limits *sdk.Limits
}

// WithLogger routes each collector's extraction warnings to a logger.
func WithLogger(l *slog.Logger) RegistryOption {
	return func(o *registryOptions) { o.logger = l }
}

// WithCollectorLimits wraps every collector in sdk.WithLimits.
//
// The limits are applied here rather than inside each engine on purpose: a limit that
// lives inside the thing being limited is not a limit. Callers that build a registry
// for production should pass this; fixture tests exercise the engines directly.
func WithCollectorLimits(l sdk.Limits) RegistryOption {
	return func(o *registryOptions) { o.limits = &l }
}

// NewRegistry builds collectors from configs and indexes them by config id.
func NewRegistry(configs []Config, opts ...RegistryOption) (*Registry, error) {
	var options registryOptions
	for _, opt := range opts {
		opt(&options)
	}

	r := &Registry{byID: make(map[string]application.Collector, len(configs))}
	var problems []error

	for _, cfg := range configs {
		id := cfg.Metadata.ID
		if existing, dup := r.byID[id]; dup {
			// Two configs claiming one id is not a merge to resolve: the id is what
			// evidence rows reference, so "which one produced this candidate" would
			// have no answer.
			problems = append(problems, fmt.Errorf(
				"collector id %q is declared twice (%s and %s); ids are referenced by evidence and must be unique",
				id, describe(existing), cfg.Path))
			continue
		}

		var (
			c   application.Collector
			err error
		)
		switch cfg.Spec.Engine {
		case EngineHTMLSelectors:
			c, err = NewHTMLSelectors(cfg, options.logger)
		case EngineTextRegex:
			c, err = NewTextRegex(cfg, options.logger)
		case EngineRSSAtom:
			c, err = NewRSSAtom(cfg, options.logger)
		default:
			err = fieldErr("spec.engine", "%q has no engine implementation", cfg.Spec.Engine)
		}
		if err != nil {
			problems = append(problems, fmt.Errorf("build collector %q from %s: %w", id, cfg.Path, err))
			continue
		}
		if options.limits != nil {
			c = sdk.WithLimits(c, *options.limits)
		}
		r.byID[id] = c
		r.order = append(r.order, id)
	}

	if len(problems) > 0 {
		return nil, errors.Join(problems...)
	}
	sort.Strings(r.order)
	return r, nil
}

// NewRegistryFromDir loads every config under root in fsys and builds a registry from
// them.
func NewRegistryFromDir(fsys fs.FS, root string, opts ...RegistryOption) (*Registry, error) {
	configs, err := LoadDir(fsys, root)
	if err != nil {
		return nil, err
	}
	return NewRegistry(configs, opts...)
}

// For resolves a source to its collector.
//
// Resolution is by the source's declared collector id, never by guessing from the URL
// or sniffing content: a source's collector is a reviewed decision recorded in
// dataset/sources/, and a registry that fell back to a plausible-looking collector
// would silently extract with the wrong rules the first time somebody made a typo.
func (r *Registry) For(src domain.Source) (application.Collector, error) {
	id := strings.TrimSpace(src.CollectorID)
	if id == "" {
		return nil, fmt.Errorf("source %s (%s) declares no collector_id: %w",
			describeSource(src), src.URL, domain.ErrNotFound)
	}
	c, ok := r.byID[id]
	if !ok {
		return nil, fmt.Errorf("source %s requests collector %q, which is not registered (known collectors: %s): %w",
			describeSource(src), id, r.known(), domain.ErrNotFound)
	}
	if !c.Supports(src) {
		return nil, fmt.Errorf("collector %q is registered but reports that it does not support source %s: %w",
			id, describeSource(src), domain.ErrNotPermitted)
	}
	return c, nil
}

// All returns every registered collector, ordered by id so that two runs enumerate
// them identically.
func (r *Registry) All() []application.Collector {
	out := make([]application.Collector, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.byID[id])
	}
	return out
}

// IDs returns the registered collector ids, in order.
func (r *Registry) IDs() []string {
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

func (r *Registry) known() string {
	if len(r.order) == 0 {
		return "none"
	}
	return strings.Join(r.order, ", ")
}

func describe(c application.Collector) string {
	return fmt.Sprintf("collector %s version %s", c.ID(), c.Version())
}

// describeSource names a source the way a maintainer reading the error would look it
// up: by id when it has one, otherwise by slug.
func describeSource(src domain.Source) string {
	switch {
	case src.ID != "" && src.Slug != "":
		return fmt.Sprintf("%s (%s)", src.ID, src.Slug)
	case src.ID != "":
		return src.ID
	case src.Slug != "":
		return src.Slug
	default:
		return "(unidentified source)"
	}
}
