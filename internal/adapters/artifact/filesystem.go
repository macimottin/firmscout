// Package artifact stores fetched content addressed by its hash.
//
// The store is content-addressed because most checks fetch a body that is byte-identical
// to the one fetched last time. Writing it again would multiply FirmScout's storage bill
// by the check frequency; recognising it costs one stat call. That is where most of the
// project's storage saving comes from, and it is why Put reports whether it created
// anything.
package artifact

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/domain"
)

// ErrInvalidHash reports a hash that is not a lower-case hexadecimal digest.
//
// The check is a security control, not tidiness: the hash becomes a filesystem path, and
// a hash containing "..", "/" or a NUL byte would let content addressing write outside
// the store. Hashes arrive from the normaliser, but the store does not get to assume
// that its only caller is the one it was written for.
var ErrInvalidHash = errors.New("artifact: hash must be lower-case hexadecimal")

const (
	objectsDir = "objects"
	objectExt  = ".gz"
	metaExt    = ".meta.json"
	// minHashLen keeps the two-level fan-out meaningful and rejects trivially short ids.
	minHashLen = 8
	maxHashLen = 128
)

// metadata is the sidecar record. It is JSON so that an operator inspecting the store
// with `cat` can read it, and so that a new field never invalidates old objects.
type metadata struct {
	Hash        string    `json:"hash"`
	ContentType string    `json:"content_type"`
	Size        int64     `json:"size"`
	Compressed  int64     `json:"compressed_size"`
	CreatedAt   time.Time `json:"created_at"`
	LastSeenAt  time.Time `json:"last_seen_at"`
}

// Store is a filesystem-backed application.ArtifactStore.
type Store struct {
	root  string
	now   func() time.Time
	perm  fs.FileMode
	dperm fs.FileMode

	mu sync.Mutex // serialises sidecar read-modify-write
}

var _ application.ArtifactStore = (*Store)(nil)

// Option configures a Store.
type Option func(*Store)

// WithClock injects a clock, which makes Touch assertions deterministic.
func WithClock(now func() time.Time) Option {
	return func(s *Store) { s.now = now }
}

// NewStore builds a store rooted at dir, creating it if necessary.
func NewStore(dir string, opts ...Option) (*Store, error) {
	s := &Store{root: dir, now: time.Now, perm: 0o640, dperm: 0o750}
	for _, o := range opts {
		o(s)
	}
	if err := os.MkdirAll(filepath.Join(s.root, objectsDir), s.dperm); err != nil {
		return nil, fmt.Errorf("artifact: creating store: %w", err)
	}
	return s, nil
}

// Put stores body under hash and returns the artifact id, which is the hash itself.
//
// created is false when the hash was already present, and in that case the body is not
// rewritten: the content is by definition identical, so rewriting it would burn IO to
// produce the same bytes. The last-seen timestamp is refreshed either way.
func (s *Store) Put(ctx context.Context, hash, contentType string, body []byte) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	if err := validateHash(hash); err != nil {
		return "", false, err
	}
	objPath := s.objectPath(hash)
	if err := os.MkdirAll(filepath.Dir(objPath), s.dperm); err != nil {
		return "", false, fmt.Errorf("artifact: creating shard: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(objPath), "."+hash+".tmp-*")
	if err != nil {
		return "", false, fmt.Errorf("artifact: creating temp object: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	zw, _ := gzip.NewWriterLevel(tmp, gzip.BestSpeed)
	if _, err := zw.Write(body); err != nil {
		_ = zw.Close()
		_ = tmp.Close()
		return "", false, fmt.Errorf("artifact: compressing: %w", err)
	}
	if err := zw.Close(); err != nil {
		_ = tmp.Close()
		return "", false, fmt.Errorf("artifact: finishing compression: %w", err)
	}
	compressed, statErr := tmp.Seek(0, io.SeekCurrent)
	if statErr != nil {
		compressed = 0
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", false, fmt.Errorf("artifact: syncing object: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", false, fmt.Errorf("artifact: closing temp object: %w", err)
	}
	if err := os.Chmod(tmpName, s.perm); err != nil {
		return "", false, fmt.Errorf("artifact: setting object mode: %w", err)
	}

	created := true
	// os.Link fails with EEXIST rather than overwriting, so two workers that fetched
	// the same body concurrently cannot both report created=true. os.Rename would
	// silently overwrite and double-count.
	if err := os.Link(tmpName, objPath); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return "", false, fmt.Errorf("artifact: linking object: %w", err)
		}
		created = false
	}

	now := s.now()
	meta := metadata{
		Hash:        hash,
		ContentType: contentType,
		Size:        int64(len(body)),
		Compressed:  compressed,
		CreatedAt:   now,
		LastSeenAt:  now,
	}
	if !created {
		if existing, err := s.readMeta(hash); err == nil {
			meta.CreatedAt = existing.CreatedAt
			if meta.ContentType == "" {
				meta.ContentType = existing.ContentType
			}
		}
	}
	if err := s.writeMeta(meta); err != nil {
		return "", false, err
	}
	return hash, created, nil
}

// Get returns the stored content, transparently decompressed.
func (s *Store) Get(ctx context.Context, id string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateHash(id); err != nil {
		return nil, err
	}
	f, err := os.Open(s.objectPath(id))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("artifact %s: %w", id, domain.ErrNotFound)
		}
		return nil, err
	}
	zr, err := gzip.NewReader(f)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("artifact %s: reading gzip stream: %w", id, err)
	}
	return &gzipReadCloser{zr: zr, file: f}, nil
}

// Touch records that the artifact was seen again, which is what lets a retention sweep
// distinguish content still in use from content no source references any more.
func (s *Store) Touch(ctx context.Context, id string, at time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateHash(id); err != nil {
		return err
	}
	if _, err := os.Stat(s.objectPath(id)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("artifact %s: %w", id, domain.ErrNotFound)
		}
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	meta, err := s.readMetaLocked(id)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		meta = metadata{Hash: id, CreatedAt: at}
	}
	meta.LastSeenAt = at
	return s.writeMetaLocked(meta)
}

// Metadata exposes the sidecar record. Retention sweeps and operator tooling read it.
func (s *Store) Metadata(id string) (contentType string, size int64, createdAt, lastSeenAt time.Time, err error) {
	if err := validateHash(id); err != nil {
		return "", 0, time.Time{}, time.Time{}, err
	}
	m, err := s.readMeta(id)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", 0, time.Time{}, time.Time{}, fmt.Errorf("artifact %s: %w", id, domain.ErrNotFound)
		}
		return "", 0, time.Time{}, time.Time{}, err
	}
	return m.ContentType, m.Size, m.CreatedAt, m.LastSeenAt, nil
}

func (s *Store) readMeta(id string) (metadata, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readMetaLocked(id)
}

func (s *Store) readMetaLocked(id string) (metadata, error) {
	var m metadata
	b, err := os.ReadFile(s.metaPath(id))
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("artifact %s: metadata is not valid json: %w", id, err)
	}
	return m, nil
}

func (s *Store) writeMeta(m metadata) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeMetaLocked(m)
}

func (s *Store) writeMetaLocked(m metadata) error {
	path := s.metaPath(m.Hash)
	if err := os.MkdirAll(filepath.Dir(path), s.dperm); err != nil {
		return fmt.Errorf("artifact: creating shard: %w", err)
	}
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("artifact: encoding metadata: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+m.Hash+".meta-*")
	if err != nil {
		return fmt.Errorf("artifact: creating temp metadata: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("artifact: writing metadata: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("artifact: closing temp metadata: %w", err)
	}
	if err := os.Chmod(tmpName, s.perm); err != nil {
		return fmt.Errorf("artifact: setting metadata mode: %w", err)
	}
	// Metadata is mutable, so rename (which replaces atomically) is correct here in a
	// way it is not for the immutable object.
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("artifact: publishing metadata: %w", err)
	}
	return nil
}

// objectPath fans the hash out over two levels so no directory holds millions of
// entries: "abcdef..." becomes "objects/ab/cd/abcdef....gz".
func (s *Store) objectPath(hash string) string {
	return filepath.Join(s.root, objectsDir, hash[0:2], hash[2:4], hash+objectExt)
}

func (s *Store) metaPath(hash string) string {
	return filepath.Join(s.root, objectsDir, hash[0:2], hash[2:4], hash+metaExt)
}

func validateHash(h string) error {
	if len(h) < minHashLen || len(h) > maxHashLen {
		return fmt.Errorf("%w: %q has length %d, want %d..%d", ErrInvalidHash, h, len(h), minHashLen, maxHashLen)
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			continue
		}
		return fmt.Errorf("%w: %q contains %q", ErrInvalidHash, h, string(rune(c)))
	}
	return nil
}

// gzipReadCloser closes both the decompressor and the underlying file. Returning only
// the gzip reader leaks a file descriptor per artifact read, which is invisible until
// the worker runs out of them.
type gzipReadCloser struct {
	zr   *gzip.Reader
	file *os.File
}

func (g *gzipReadCloser) Read(p []byte) (int, error) { return g.zr.Read(p) }

func (g *gzipReadCloser) Close() error {
	zerr := g.zr.Close()
	ferr := g.file.Close()
	if zerr != nil {
		return zerr
	}
	return ferr
}

// PutBlob stores content addressed by its hash, satisfying application.BlobStore.
//
// Store predates the split between artifact metadata and artifact bytes and still
// satisfies application.ArtifactStore for callers that want a database-free store,
// such as a CLI run against a scratch directory. These three methods expose the same
// filesystem as the narrower blob port, so the PostgreSQL-backed metadata store can
// delegate to it.
func (s *Store) PutBlob(ctx context.Context, hash string, body []byte) (string, bool, error) {
	_, created, err := s.Put(ctx, hash, "", body)
	if err != nil {
		return "", false, err
	}
	// The key is the hash: content-addressed storage needs no separate identifier,
	// and using the hash means the same bytes are never written twice.
	return hash, created, nil
}

// GetBlob returns the bytes stored under a key.
func (s *Store) GetBlob(ctx context.Context, key string) (io.ReadCloser, error) {
	return s.Get(ctx, key)
}

// Backend names this implementation for the storage_backend column.
func (s *Store) Backend() string { return "filesystem" }
