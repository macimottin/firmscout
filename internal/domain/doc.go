// Package domain contains FirmScout's business rules: entities, value objects, state
// machines and pure domain services.
//
// The dependency rule for this package is absolute and enforced by internal/archtest:
// it imports the Go standard library and nothing else. No database driver, no HTTP
// client, no AWS SDK, no telemetry library, no YAML parser. If a concept here needs a
// clock, an identifier generator or persistence, the application layer supplies it.
//
// The package is deliberately flat rather than split into one package per entity.
// Releases reference products, candidates reference sources and products, and sources
// reference vendors, so splitting them would either create import cycles or force a
// layer of indirection that buys nothing. The bounded contexts described in the
// blueprint are expressed by file boundaries and naming, not by Go packages.
package domain
