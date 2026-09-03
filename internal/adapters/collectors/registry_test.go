package collectors_test

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/macimottin/firmscout/internal/adapters/collectors"
	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/domain"
)

func newShippedRegistry(t *testing.T) *collectors.Registry {
	t.Helper()
	r, err := collectors.NewRegistryFromDir(os.DirFS(repoRoot), configRoot)
	if err != nil {
		t.Fatalf("build registry from %s: %v", configRoot, err)
	}
	return r
}

// TestRegistrySatisfiesThePort keeps the adapter and the port from drifting apart at
// compile time rather than at the first scheduled check.
func TestRegistrySatisfiesThePort(t *testing.T) {
	var _ application.CollectorRegistry = newShippedRegistry(t)
}

func TestRegistryResolvesBySourceCollectorID(t *testing.T) {
	r := newShippedRegistry(t)

	for _, id := range []string{"mikrotik.changelogs", "mikrotik.newest-stable"} {
		src := domain.Source{ID: "src_1", Slug: "changelogs", CollectorID: id}
		c, err := r.For(src)
		if err != nil {
			t.Fatalf("resolve %s: %v", id, err)
		}
		if c.ID() != id {
			t.Errorf("resolved collector id: want %q, got %q", id, c.ID())
		}
		if c.Vendor() != "mikrotik" {
			t.Errorf("vendor: want mikrotik, got %q", c.Vendor())
		}
		if c.Version() == "" {
			t.Errorf("collector %s reports no version", id)
		}
	}

	if got := len(r.All()); got < 2 {
		t.Errorf("All(): want at least 2 collectors, got %d", got)
	}
	ids := r.IDs()
	for i := 1; i < len(ids); i++ {
		if ids[i-1] > ids[i] {
			t.Errorf("IDs() is not sorted: %v", ids)
			break
		}
	}
}

// TestRegistryNamesTheSourceAndTheMissingCollector: the error a maintainer reads at
// 3am must say which source asked for which collector, not "not found".
func TestRegistryNamesTheSourceAndTheMissingCollector(t *testing.T) {
	r := newShippedRegistry(t)

	src := domain.Source{ID: "src_42", Slug: "downloads", CollectorID: "mikrotik.downloads"}
	_, err := r.For(src)
	if err == nil {
		t.Fatal("resolving an unregistered collector id succeeded")
	}
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("error does not classify as not-found: %v", err)
	}
	for _, want := range []string{"src_42", "downloads", "mikrotik.downloads", "mikrotik.changelogs"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err.Error(), want)
		}
	}
}

func TestRegistryRejectsASourceWithNoCollectorID(t *testing.T) {
	r := newShippedRegistry(t)
	_, err := r.For(domain.Source{ID: "src_7", Slug: "orphan", URL: "https://example.test/x"})
	if err == nil {
		t.Fatal("a source with no collector id resolved to something")
	}
	if !strings.Contains(err.Error(), "declares no collector_id") {
		t.Errorf("error does not explain the problem: %v", err)
	}
}

// TestRegistryRejectsDuplicateIDs: two configs claiming one id is not a merge to
// resolve. The id is what evidence rows reference, so "which one produced this
// candidate" would have no answer.
func TestRegistryRejectsDuplicateIDs(t *testing.T) {
	cfg, err := collectors.LoadConfig([]byte(validConfig))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	first, second := cfg, cfg
	first.Path = "a.yaml"
	second.Path = "b.yaml"

	_, err = collectors.NewRegistry([]collectors.Config{first, second})
	if err == nil {
		t.Fatal("two configs with the same id built a registry")
	}
	if !strings.Contains(err.Error(), "declared twice") {
		t.Errorf("error does not explain the duplicate: %v", err)
	}
	if !strings.Contains(err.Error(), "b.yaml") {
		t.Errorf("error does not name the offending file: %v", err)
	}
}

// TestSupportsIsRoutingOnly: Supports must be cheap and must not accept a source
// routed to a different collector.
func TestSupportsRejectsAForeignSource(t *testing.T) {
	c := newShippedHTMLCollector(t, "mikrotik.changelogs")
	if c.Supports(domain.Source{CollectorID: "mikrotik.newest-stable"}) {
		t.Error("the changelog collector claims to support the pointer-file source")
	}
	if !c.Supports(domain.Source{CollectorID: "mikrotik.changelogs"}) {
		t.Error("the changelog collector does not support its own source")
	}
}
