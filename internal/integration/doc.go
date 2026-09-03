// Package integration wires the whole of FirmScout together for end-to-end tests.
//
// It contains no production code and nothing imports it. Its purpose is to prove that
// the pieces verified in isolation -- the domain's rules, the use cases against fakes,
// each adapter against its own contract -- actually compose into a working pipeline
// when connected to a real PostgreSQL and a real HTTP server.
//
// The vendor site is the one thing that is NOT real here. Every fetch in these tests
// hits a local httptest server replaying a recorded fixture, because a test suite that
// fails when MikroTik redeploys its website is a test suite people learn to ignore.
package integration
