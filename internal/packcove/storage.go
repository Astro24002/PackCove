package packcove

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

var errCacheMiss = errors.New("cache entry not found")

type CacheEntry struct {
	Repository      string
	Key             string
	BlobHash        string
	Size            int64
	ContentType     string
	ContentEncoding string
	Vary            string
	CacheControl    string
	ETag            string
	LastModified    string
	ValidatedAt     time.Time
	FreshUntil      time.Time
	UpstreamURL     string
}

type DownloadMapping struct {
	ID                string
	Repository        string
	UpstreamURL       string
	Filename          string
	ExpectedAlgorithm string
	ExpectedDigest    string
	MetadataAlgorithm string
	MetadataDigest    string
}

type Storage struct {
	root string
	db   *sql.DB
}

func OpenStorage(root string) (*Storage, error) {
	for _, dir := range []string{root, filepath.Join(root, "blobs"), filepath.Join(root, "tmp")} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("create storage directory %q: %w", dir, err)
		}
	}
	db, err := sql.Open("sqlite", filepath.Join(root, "index.sqlite"))
	if err != nil {
		return nil, fmt.Errorf("open cache index: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("configure cache index: %w", err)
	}
	_, err = db.Exec(`
CREATE TABLE IF NOT EXISTS cache_entries (
 repository TEXT NOT NULL,
 cache_key TEXT NOT NULL,
 blob_hash TEXT NOT NULL,
 size INTEGER NOT NULL,
 content_type TEXT NOT NULL,
 content_encoding TEXT NOT NULL,
 vary TEXT NOT NULL,
 cache_control TEXT NOT NULL,
 etag TEXT NOT NULL,
 last_modified TEXT NOT NULL,
 validated_at INTEGER NOT NULL,
 fresh_until INTEGER NOT NULL,
 upstream_url TEXT NOT NULL,
 PRIMARY KEY (repository, cache_key)
);
CREATE TABLE IF NOT EXISTS download_mappings (
 id TEXT PRIMARY KEY,
 repository TEXT NOT NULL,
 upstream_url TEXT NOT NULL,
 filename TEXT NOT NULL,
 expected_algorithm TEXT NOT NULL,
 expected_digest TEXT NOT NULL,
 metadata_algorithm TEXT NOT NULL,
 metadata_digest TEXT NOT NULL
);
`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("initialize cache index: %w", err)
	}
	return &Storage{root: root, db: db}, nil
}

func (s *Storage) Close() error { return s.db.Close() }

func (s *Storage) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *Storage) Get(ctx context.Context, repository, key string) (CacheEntry, string, error) {
	var entry CacheEntry
	var validatedAt, freshUntil int64
	err := s.db.QueryRowContext(ctx, `SELECT repository, cache_key, blob_hash, size, content_type, content_encoding, vary, cache_control, etag, last_modified, validated_at, fresh_until, upstream_url FROM cache_entries WHERE repository=? AND cache_key=?`, repository, key).Scan(
		&entry.Repository, &entry.Key, &entry.BlobHash, &entry.Size, &entry.ContentType, &entry.ContentEncoding, &entry.Vary, &entry.CacheControl, &entry.ETag, &entry.LastModified, &validatedAt, &freshUntil, &entry.UpstreamURL,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return CacheEntry{}, "", errCacheMiss
	}
	if err != nil {
		return CacheEntry{}, "", err
	}
	entry.ValidatedAt = time.Unix(0, validatedAt)
	if freshUntil == 0 {
		entry.FreshUntil = time.Time{}
	} else {
		entry.FreshUntil = time.Unix(0, freshUntil)
	}
	path := filepath.Join(s.root, "blobs", entry.BlobHash)
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			_, _ = s.db.ExecContext(ctx, `DELETE FROM cache_entries WHERE repository=? AND cache_key=?`, repository, key)
			return CacheEntry{}, "", errCacheMiss
		}
		return CacheEntry{}, "", err
	}
	return entry, path, nil
}

func (s *Storage) Store(ctx context.Context, entry CacheEntry, body io.Reader, expectedLength int64, digestAlgorithm, expectedDigest string) (CacheEntry, string, error) {
	tmp, err := os.CreateTemp(filepath.Join(s.root, "tmp"), "download-*")
	if err != nil {
		return CacheEntry{}, "", fmt.Errorf("create temporary cache file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	checksum := sha256.New()
	var verifier hash.Hash
	switch strings.ToLower(digestAlgorithm) {
	case "":
	case "sha1":
		verifier = sha1.New()
	case "sha256":
		verifier = sha256.New()
	case "sha384":
		verifier = sha512.New384()
	case "sha512":
		verifier = sha512.New()
	default:
		tmp.Close()
		return CacheEntry{}, "", fmt.Errorf("unsupported integrity algorithm %q", digestAlgorithm)
	}
	writers := []io.Writer{tmp, checksum}
	if verifier != nil {
		writers = append(writers, verifier)
	}
	size, copyErr := io.Copy(io.MultiWriter(writers...), body)
	if copyErr != nil {
		tmp.Close()
		return CacheEntry{}, "", fmt.Errorf("write cache file: %w", copyErr)
	}
	if expectedLength >= 0 && size != expectedLength {
		tmp.Close()
		return CacheEntry{}, "", fmt.Errorf("upstream length mismatch: expected %d bytes, received %d", expectedLength, size)
	}
	if verifier != nil && !strings.EqualFold(hex.EncodeToString(verifier.Sum(nil)), expectedDigest) {
		tmp.Close()
		return CacheEntry{}, "", fmt.Errorf("upstream digest mismatch: expected %s, received %s", expectedDigest, hex.EncodeToString(verifier.Sum(nil)))
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return CacheEntry{}, "", fmt.Errorf("sync cache file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return CacheEntry{}, "", fmt.Errorf("close cache file: %w", err)
	}
	entry.BlobHash = hex.EncodeToString(checksum.Sum(nil))
	entry.Size = size
	blobPath := filepath.Join(s.root, "blobs", entry.BlobHash)
	if err := os.Rename(tmpName, blobPath); err != nil {
		if !errors.Is(err, os.ErrExist) {
			if _, statErr := os.Stat(blobPath); statErr != nil {
				return CacheEntry{}, "", fmt.Errorf("publish cache file: %w", err)
			}
		}
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO cache_entries(repository, cache_key, blob_hash, size, content_type, content_encoding, vary, cache_control, etag, last_modified, validated_at, fresh_until, upstream_url) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(repository, cache_key) DO UPDATE SET blob_hash=excluded.blob_hash,size=excluded.size,content_type=excluded.content_type,content_encoding=excluded.content_encoding,vary=excluded.vary,cache_control=excluded.cache_control,etag=excluded.etag,last_modified=excluded.last_modified,validated_at=excluded.validated_at,fresh_until=excluded.fresh_until,upstream_url=excluded.upstream_url`,
		entry.Repository, entry.Key, entry.BlobHash, entry.Size, entry.ContentType, entry.ContentEncoding, entry.Vary, entry.CacheControl, entry.ETag, entry.LastModified, entry.ValidatedAt.UnixNano(), unixNanoOrZero(entry.FreshUntil), entry.UpstreamURL,
	)
	if err != nil {
		return CacheEntry{}, "", fmt.Errorf("index cached content: %w", err)
	}
	return entry, blobPath, nil
}

func (s *Storage) Revalidate(ctx context.Context, entry CacheEntry, ttl time.Duration) (CacheEntry, error) {
	now := time.Now().UTC()
	entry.ValidatedAt = now
	entry.FreshUntil = time.Time{}
	if ttl > 0 {
		entry.FreshUntil = now.Add(ttl)
	}
	_, err := s.db.ExecContext(ctx, `UPDATE cache_entries SET validated_at=?, fresh_until=? WHERE repository=? AND cache_key=?`, unixNanoOrZero(entry.ValidatedAt), unixNanoOrZero(entry.FreshUntil), entry.Repository, entry.Key)
	if err != nil {
		return CacheEntry{}, fmt.Errorf("update cache validation time: %w", err)
	}
	return entry, nil
}

func (s *Storage) MapDownload(ctx context.Context, repository, upstreamURL, filename, expectedAlgorithm, expectedDigest, metadataAlgorithm, metadataDigest string) (string, error) {
	sum := sha256.Sum256([]byte(repository + "\n" + upstreamURL + "\n" + expectedAlgorithm + "\n" + expectedDigest + "\n" + metadataAlgorithm + "\n" + metadataDigest))
	id := hex.EncodeToString(sum[:])
	_, err := s.db.ExecContext(ctx, `INSERT INTO download_mappings(id,repository,upstream_url,filename,expected_algorithm,expected_digest,metadata_algorithm,metadata_digest) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING`, id, repository, upstreamURL, filename, expectedAlgorithm, expectedDigest, metadataAlgorithm, metadataDigest)
	if err != nil {
		return "", fmt.Errorf("store download mapping: %w", err)
	}
	return id, nil
}

func (s *Storage) ResolveDownload(ctx context.Context, id string) (DownloadMapping, error) {
	var mapping DownloadMapping
	err := s.db.QueryRowContext(ctx, `SELECT id, repository, upstream_url, filename, expected_algorithm, expected_digest, metadata_algorithm, metadata_digest FROM download_mappings WHERE id=?`, id).Scan(&mapping.ID, &mapping.Repository, &mapping.UpstreamURL, &mapping.Filename, &mapping.ExpectedAlgorithm, &mapping.ExpectedDigest, &mapping.MetadataAlgorithm, &mapping.MetadataDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return DownloadMapping{}, errCacheMiss
	}
	return mapping, err
}

func (s *Storage) TotalBytes(ctx context.Context) (int64, error) {
	var total int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(size),0) FROM (SELECT DISTINCT blob_hash,size FROM cache_entries)`).Scan(&total)
	return total, err
}

func (s *Storage) FreeBytes() (int64, error) {
	var stats syscall.Statfs_t
	if err := syscall.Statfs(s.root, &stats); err != nil {
		return 0, err
	}
	return int64(stats.Bavail) * int64(stats.Bsize), nil
}

func unixNanoOrZero(value time.Time) int64 {
	if value.IsZero() {
		return 0
	}
	return value.UnixNano()
}
