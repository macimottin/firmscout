package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/macimottin/firmscout/internal/domain"
)

func hashOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func newStore(t *testing.T, opts ...Option) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPutIsContentAddressedAndIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)

	body := []byte(`{"version":"7.25.1","channel":"stable"}`)
	h := hashOf(body)

	id, created, err := s.Put(ctx, h, "application/json", body)
	if err != nil {
		t.Fatalf("first Put: %v", err)
	}
	if !created {
		t.Fatal("first Put reported created=false")
	}
	if id != h {
		t.Fatalf("id = %q, want the hash %q", id, h)
	}

	id2, created2, err := s.Put(ctx, h, "application/json", body)
	if err != nil {
		t.Fatalf("second Put: %v", err)
	}
	if created2 {
		t.Fatal("second Put of identical content reported created=true; the storage saving depends on this")
	}
	if id2 != id {
		t.Fatalf("id changed between puts: %q then %q", id, id2)
	}

	rc, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = rc.Close() }()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("Get returned %q, want %q", got, body)
	}
}

func TestStoredObjectIsGzippedAndShardedByHash(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	s, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}

	body := bytes.Repeat([]byte("RouterOS 7.25.1 changelog entry\n"), 512)
	h := hashOf(body)
	if _, _, err := s.Put(ctx, h, "text/plain", body); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(root, objectsDir, h[0:2], h[2:4], h+objectExt)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("object is not at the content-addressed path %s: %v", path, err)
	}
	if len(raw) < 2 || raw[0] != 0x1f || raw[1] != 0x8b {
		t.Fatal("stored object is not gzip-compressed")
	}
	if len(raw) >= len(body) {
		t.Fatalf("compression saved nothing: %d stored for %d bytes", len(raw), len(body))
	}
}

func TestGetMissingReportsNotFound(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	_, err := s.Get(context.Background(), hashOf([]byte("absent")))
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("err = %v, want domain.ErrNotFound", err)
	}
}

func TestTouchUpdatesLastSeen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	created := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	s := newStore(t, WithClock(func() time.Time { return created }))

	body := []byte("7.25.1")
	h := hashOf(body)
	if _, _, err := s.Put(ctx, h, "text/plain", body); err != nil {
		t.Fatal(err)
	}

	_, _, gotCreated, gotSeen, err := s.Metadata(h)
	if err != nil {
		t.Fatal(err)
	}
	if !gotCreated.Equal(created) || !gotSeen.Equal(created) {
		t.Fatalf("created=%v seen=%v, want both %v", gotCreated, gotSeen, created)
	}

	later := created.Add(72 * time.Hour)
	if err := s.Touch(ctx, h, later); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	ct, size, gotCreated, gotSeen, err := s.Metadata(h)
	if err != nil {
		t.Fatal(err)
	}
	if !gotSeen.Equal(later) {
		t.Fatalf("last seen = %v, want %v", gotSeen, later)
	}
	if !gotCreated.Equal(created) {
		t.Fatalf("Touch rewrote created_at to %v", gotCreated)
	}
	if ct != "text/plain" || size != int64(len(body)) {
		t.Fatalf("metadata content type=%q size=%d", ct, size)
	}
}

func TestTouchMissingReportsNotFound(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	err := s.Touch(context.Background(), hashOf([]byte("absent")), time.Now())
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("err = %v, want domain.ErrNotFound", err)
	}
}

func TestPutPreservesCreatedAtOnRepeat(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	s := newStore(t, WithClock(func() time.Time { return now }))

	body := []byte("7.25.1")
	h := hashOf(body)
	if _, _, err := s.Put(ctx, h, "text/plain", body); err != nil {
		t.Fatal(err)
	}
	first := now
	now = now.Add(24 * time.Hour)
	if _, created, err := s.Put(ctx, h, "text/plain", body); err != nil || created {
		t.Fatalf("second Put: created=%v err=%v", created, err)
	}
	_, _, gotCreated, gotSeen, err := s.Metadata(h)
	if err != nil {
		t.Fatal(err)
	}
	if !gotCreated.Equal(first) {
		t.Fatalf("created_at = %v, want the original %v", gotCreated, first)
	}
	if !gotSeen.Equal(now) {
		t.Fatalf("last_seen_at = %v, want %v", gotSeen, now)
	}
}

// TestHashValidationRejectsPathEscapes is a security test, not a tidiness one: the hash
// becomes a filesystem path.
func TestHashValidationRejectsPathEscapes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	s, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}

	bad := []string{
		"",
		"abc",
		"../../../../etc/passwd",
		"ab/cd/ef01",
		"ABCDEF0123456789",
		"abcdef012345678g",
		"abcdef01\x00",
		strings.Repeat("a", maxHashLen+1),
	}
	for _, h := range bad {
		if _, _, err := s.Put(ctx, h, "text/plain", []byte("x")); !errors.Is(err, ErrInvalidHash) {
			t.Errorf("Put(%q) = %v, want ErrInvalidHash", h, err)
		}
		if _, err := s.Get(ctx, h); !errors.Is(err, ErrInvalidHash) {
			t.Errorf("Get(%q) = %v, want ErrInvalidHash", h, err)
		}
		if err := s.Touch(ctx, h, time.Now()); !errors.Is(err, ErrInvalidHash) {
			t.Errorf("Touch(%q) = %v, want ErrInvalidHash", h, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "..", "..", "etc")); err == nil {
		t.Fatal("a traversal attempt created something outside the store")
	}
}

func TestConcurrentPutReportsCreatedExactlyOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)

	body := []byte("concurrent content-addressed body")
	h := hashOf(body)

	const n = 16
	var mu sync.Mutex
	creations := 0
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, created, err := s.Put(ctx, h, "text/plain", body)
			if err != nil {
				t.Error(err)
				return
			}
			if created {
				mu.Lock()
				creations++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if creations != 1 {
		t.Fatalf("created=true was reported %d times across %d concurrent puts, want exactly 1", creations, n)
	}

	rc, err := s.Get(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("content was corrupted by concurrent writers: %q", got)
	}
}

func TestPutLeavesNoTempFiles(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	s, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("7.25.1")
	h := hashOf(body)
	for i := 0; i < 3; i++ {
		if _, _, err := s.Put(ctx, h, "text/plain", body); err != nil {
			t.Fatal(err)
		}
	}
	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasPrefix(info.Name(), ".") {
			t.Errorf("temporary file left behind: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestEmptyBodyRoundTrips(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)
	body := []byte{}
	h := hashOf(body)
	if _, created, err := s.Put(ctx, h, "text/plain", body); err != nil || !created {
		t.Fatalf("Put: created=%v err=%v", created, err)
	}
	rc, err := s.Get(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("got %q, want empty", got)
	}
}

func TestCancelledContextIsRespected(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := newStore(t)
	body := []byte("x")
	h := hashOf(body)
	if _, _, err := s.Put(ctx, h, "text/plain", body); !errors.Is(err, context.Canceled) {
		t.Errorf("Put = %v, want context.Canceled", err)
	}
	if _, err := s.Get(ctx, h); !errors.Is(err, context.Canceled) {
		t.Errorf("Get = %v, want context.Canceled", err)
	}
	if err := s.Touch(ctx, h, time.Now()); !errors.Is(err, context.Canceled) {
		t.Errorf("Touch = %v, want context.Canceled", err)
	}
}
