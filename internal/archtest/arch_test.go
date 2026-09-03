// Package archtest enforces FirmScout's Clean Architecture dependency rule.
//
// The rule is not a convention documented in a file nobody reads. It is a test that
// fails the build. Every layer's permitted imports are declared below, and the test
// walks the real import graph with `go list` to prove no forbidden edge exists.
//
// See ADR-0002 and docs/architecture/clean-architecture.md.
package archtest

import (
	"encoding/json"
	"os/exec"
	"sort"
	"strings"
	"testing"
)

const modulePath = "github.com/macimottin/firmscout"

// pkg is the subset of `go list -json` output this test needs.
type pkg struct {
	ImportPath string
	Imports    []string
	Standard   bool
}

// listPackages returns every package in the module with its direct imports.
func listPackages(t *testing.T) []pkg {
	t.Helper()
	cmd := exec.Command("go", "list", "-json", "./...")
	cmd.Dir = ".."
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("go list failed: %v\n%s", err, ee.Stderr)
		}
		t.Fatalf("go list failed: %v", err)
	}
	var pkgs []pkg
	dec := json.NewDecoder(strings.NewReader(string(out)))
	for dec.More() {
		var p pkg
		if err := dec.Decode(&p); err != nil {
			t.Fatalf("decode go list output: %v", err)
		}
		pkgs = append(pkgs, p)
	}
	if len(pkgs) == 0 {
		t.Fatal("go list returned no packages")
	}
	return pkgs
}

// isStdlib reports whether an import path names a standard library package. Standard
// library paths have no dot in their first segment.
func isStdlib(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}

// The domain is the innermost layer. It models what FirmScout would still be if it
// were run on paper: vendors, sources, candidates, releases, evidence, versions and
// dates. Nothing about how those are stored, served or fetched belongs here, so the
// domain may import the standard library and nothing else.
//
// This is the strictest rule in the project and the one that keeps the business logic
// testable in microseconds and portable across every infrastructure decision the
// project has not yet made.
func TestDomainImportsOnlyStandardLibrary(t *testing.T) {
	var violations []string
	for _, p := range listPackages(t) {
		if !strings.HasPrefix(p.ImportPath, modulePath+"/internal/domain") {
			continue
		}
		for _, imp := range p.Imports {
			if isStdlib(imp) {
				continue
			}
			if strings.HasPrefix(imp, modulePath+"/internal/domain") {
				continue
			}
			violations = append(violations, p.ImportPath+" imports "+imp)
		}
	}
	report(t, "internal/domain may import only the Go standard library", violations)
}

// The application layer owns the use cases and defines the ports they need. It may
// depend on the domain, because use cases orchestrate domain objects. It may not
// depend on any adapter, on the platform wiring, or on any third-party library,
// because a use case that imports pgx or the AWS SDK has stopped being a business rule
// and become infrastructure.
//
// The apptest subpackage is included in this rule deliberately: the in-memory fakes are
// part of the application's own test surface and must not reach for a real adapter.
func TestApplicationImportsOnlyDomainAndStandardLibrary(t *testing.T) {
	var violations []string
	for _, p := range listPackages(t) {
		if !strings.HasPrefix(p.ImportPath, modulePath+"/internal/application") {
			continue
		}
		for _, imp := range p.Imports {
			switch {
			case isStdlib(imp):
			case strings.HasPrefix(imp, modulePath+"/internal/domain"):
			case strings.HasPrefix(imp, modulePath+"/internal/application"):
			default:
				violations = append(violations, p.ImportPath+" imports "+imp)
			}
		}
	}
	report(t, "internal/application may import only internal/domain and the Go standard library", violations)
}

// Adapters translate between the application's ports and a specific technology. They
// may import the domain, the application and their own third-party library. They may
// not import internal/platform, because platform is the composition root: an adapter
// that reaches into the wiring has inverted the dependency it exists to satisfy.
//
// Adapters also may not import each other. The PostgreSQL adapter has no business
// knowing that an HTTP adapter exists; if two adapters need to share code, that code
// belongs in the application layer as a port, or in a package neither owns.
func TestAdaptersDoNotImportPlatformOrEachOther(t *testing.T) {
	var violations []string
	for _, p := range listPackages(t) {
		const adapterRoot = modulePath + "/internal/adapters/"
		if !strings.HasPrefix(p.ImportPath, adapterRoot) {
			continue
		}
		self := adapterPackage(p.ImportPath)
		for _, imp := range p.Imports {
			if strings.HasPrefix(imp, modulePath+"/internal/platform") {
				violations = append(violations, p.ImportPath+" imports "+imp+" (adapters must not reach into the composition root)")
				continue
			}
			if strings.HasPrefix(imp, adapterRoot) && adapterPackage(imp) != self {
				violations = append(violations, p.ImportPath+" imports "+imp+" (adapters must not depend on each other)")
			}
		}
	}
	report(t, "adapters may not import internal/platform or each other", violations)
}

// adapterPackage returns the first path segment under internal/adapters, so that
// internal/adapters/postgres/foo and internal/adapters/postgres both resolve to
// "postgres".
func adapterPackage(importPath string) string {
	const adapterRoot = modulePath + "/internal/adapters/"
	rest := strings.TrimPrefix(importPath, adapterRoot)
	first, _, _ := strings.Cut(rest, "/")
	return first
}

// Nothing outside internal/adapters may import an adapter, except internal/platform
// and the application binaries. This is the rule that keeps the dependency arrows
// pointing inward: the domain and the application are unaware that PostgreSQL, HTTP or
// OpenTelemetry exist.
func TestOnlyPlatformAndAppsImportAdapters(t *testing.T) {
	var violations []string
	for _, p := range listPackages(t) {
		if strings.HasPrefix(p.ImportPath, modulePath+"/internal/adapters") {
			continue
		}
		if strings.HasPrefix(p.ImportPath, modulePath+"/internal/platform") {
			continue
		}
		if strings.HasPrefix(p.ImportPath, modulePath+"/apps/") {
			continue
		}
		if strings.HasPrefix(p.ImportPath, modulePath+"/internal/integration") {
			// The integration package is a second composition root that exists
			// only to assemble the whole system for an end-to-end test. It
			// contains no production code and nothing imports it.
			continue
		}
		if strings.HasPrefix(p.ImportPath, modulePath+"/collectors") {
			// The collector SDK is a public extension point that adapters implement
			// against; it is allowed to reference the collector adapter packages.
			continue
		}
		for _, imp := range p.Imports {
			if strings.HasPrefix(imp, modulePath+"/internal/adapters") {
				violations = append(violations, p.ImportPath+" imports "+imp)
			}
		}
	}
	report(t, "only internal/platform and apps/* may import adapters", violations)
}

// A collector must not be able to write a release. The strongest available guarantee
// short of a separate process is that collector packages never import a repository
// implementation, so there is no handle through which they could persist anything.
//
// See ADR-0005: collectors emit candidates, and a separate use case publishes.
func TestCollectorsCannotReachPersistence(t *testing.T) {
	var violations []string
	for _, p := range listPackages(t) {
		isCollector := strings.HasPrefix(p.ImportPath, modulePath+"/collectors") ||
			strings.HasPrefix(p.ImportPath, modulePath+"/internal/adapters/collectors")
		if !isCollector {
			continue
		}
		for _, imp := range p.Imports {
			if strings.HasPrefix(imp, modulePath+"/internal/adapters/postgres") {
				violations = append(violations, p.ImportPath+" imports "+imp+" (a collector must not be able to persist anything)")
			}
		}
	}
	report(t, "collectors must not import a persistence adapter", violations)
}

// The domain must not depend on a specific observability vendor either. Instrumentation
// happens at adapter boundaries or through a port; a domain package importing an
// OpenTelemetry SDK would make the business rules depend on a telemetry decision.
func TestDomainAndApplicationAreTelemetryAgnostic(t *testing.T) {
	forbidden := []string{
		"go.opentelemetry.io/",
		"github.com/prometheus/",
		"github.com/jackc/",
		"gopkg.in/yaml",
		"github.com/PuerkitoBio/",
	}
	var violations []string
	for _, p := range listPackages(t) {
		inner := strings.HasPrefix(p.ImportPath, modulePath+"/internal/domain") ||
			strings.HasPrefix(p.ImportPath, modulePath+"/internal/application")
		if !inner {
			continue
		}
		for _, imp := range p.Imports {
			for _, f := range forbidden {
				if strings.HasPrefix(imp, f) {
					violations = append(violations, p.ImportPath+" imports "+imp)
				}
			}
		}
	}
	report(t, "domain and application must not import an infrastructure library", violations)
}

// The module must contain a domain and an application package. A refactor that
// accidentally removed them would otherwise make every rule above pass vacuously.
func TestLayersExist(t *testing.T) {
	var hasDomain, hasApplication bool
	for _, p := range listPackages(t) {
		if p.ImportPath == modulePath+"/internal/domain" {
			hasDomain = true
		}
		if p.ImportPath == modulePath+"/internal/application" {
			hasApplication = true
		}
	}
	if !hasDomain {
		t.Error("internal/domain does not exist; the dependency rules above would pass vacuously")
	}
	if !hasApplication {
		t.Error("internal/application does not exist; the dependency rules above would pass vacuously")
	}
}

// report fails the test with every violation listed, sorted, so a developer sees the
// whole problem rather than fixing one edge at a time.
func report(t *testing.T, rule string, violations []string) {
	t.Helper()
	if len(violations) == 0 {
		return
	}
	sort.Strings(violations)
	t.Errorf("dependency rule violated: %s\n\n  %s\n\nSee ADR-0002 and docs/architecture/clean-architecture.md.",
		rule, strings.Join(violations, "\n  "))
}
