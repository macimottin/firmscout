package platform

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/binary"
	"sync"
	"time"
)

// SystemClock is the real clock.
type SystemClock struct{}

// Now returns the current UTC time. Everything in FirmScout works in UTC; local time
// zones are a presentation concern and never reach the database.
func (SystemClock) Now() time.Time { return time.Now().UTC() }

// IDGenerator produces sortable, prefixed identifiers such as "rel_01K3M8QF2X7B9WVTZC".
//
// The format is a ULID-like 48-bit millisecond timestamp followed by 80 bits of
// randomness, encoded in Crockford-style base32. Three properties matter:
// identifiers sort by creation time, which makes index locality good and log reading
// pleasant; the prefix says what the identifier refers to without a lookup; and
// generation needs no database round trip, so the domain can construct an entity
// before any transaction is open.
type IDGenerator struct {
	mu   sync.Mutex
	last uint64
}

var idEncoding = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

// NewIDGenerator returns a generator.
func NewIDGenerator() *IDGenerator { return &IDGenerator{} }

// NewID returns a new identifier with the given type prefix.
func (g *IDGenerator) NewID(prefix string) string {
	g.mu.Lock()
	defer g.mu.Unlock()

	ms := uint64(time.Now().UTC().UnixMilli())
	if ms <= g.last {
		// Two identifiers requested inside the same millisecond still sort in
		// creation order.
		ms = g.last + 1
	}
	g.last = ms

	var buf [16]byte
	binary.BigEndian.PutUint64(buf[:8], ms<<16)
	if _, err := rand.Read(buf[6:]); err != nil {
		// crypto/rand failing is not a recoverable condition for a process that
		// needs unique identifiers, but panicking in a library call is worse than
		// degrading: fall back to a counter-derived suffix.
		binary.BigEndian.PutUint64(buf[8:], ms*2654435761)
	}
	return prefix + "_" + idEncoding.EncodeToString(buf[:])
}
