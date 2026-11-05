package pg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lineahq/linea/internal/debugger/artifact"
)

var _ artifact.Transfers = (*Store)(nil)

// stagedMaxAge is how long a staged object may stay before Sweep deletes it.
// Staged objects are never referenced by rows, so no age is a safe margin for
// them other than the URL lifetimes the platform hands out.
const stagedMaxAge = time.Hour

// maxStagedName bounds a staged object's name.
const maxStagedName = 128

// stagingPrefix is the key prefix of the installation's staged objects. It
// shares no prefix with objectPrefix, so Sweep's digest listing never sees
// them.
func stagingPrefix(prefix string) string {
	if prefix == "" {
		return "staging/"
	}
	return prefix + "/staging/"
}

// stagingKey is <prefix>/staging/<name>. A name is 1 to 128 lower-case letters,
// digits and '-'.
func stagingKey(prefix, name string) (string, error) {
	if name == "" || len(name) > maxStagedName {
		return "", invalid("invalid staged object name")
	}
	for _, c := range []byte(name) {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return "", invalid("invalid staged object name")
		}
	}
	return stagingPrefix(prefix) + name, nil
}

// RunBundleURL presigns a read of a run's input bundle.
func (s *Store) RunBundleURL(ctx context.Context, id string, lifetime time.Duration) (artifact.ObjectURL, error) {
	if err := s.ready(ctx); err != nil {
		return artifact.ObjectURL{}, err
	}
	if !validRunID(id) || lifetime <= 0 {
		return artifact.ObjectURL{}, invalid("invalid bundle URL request")
	}
	var b blob
	err := s.pool.QueryRow(ctx, "SELECT bundle_digest, bundle_bytes FROM runs WHERE id = $1", id).Scan(&b.digest, &b.size)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && b.digest == "" {
		return artifact.ObjectURL{}, artifact.ErrNotFound
	}
	if err != nil {
		return artifact.ObjectURL{}, storageErr("run bundle", err)
	}
	key, err := objectKey(s.prefix, b.digest)
	if err != nil {
		return artifact.ObjectURL{}, err
	}
	u, err := s.objects.PresignGet(ctx, key, lifetime)
	if err != nil {
		return artifact.ObjectURL{}, s.presignErr(ctx, err)
	}
	return artifact.ObjectURL{URL: u, Digest: b.digest, Size: b.size}, nil
}

// StageURL presigns a write of one staged object.
func (s *Store) StageURL(ctx context.Context, name string, lifetime time.Duration) (string, error) {
	if err := s.ready(ctx); err != nil {
		return "", err
	}
	key, err := stagingKey(s.prefix, name)
	if err != nil {
		return "", err
	}
	if lifetime <= 0 {
		return "", invalid("invalid staged URL lifetime")
	}
	u, err := s.objects.PresignPut(ctx, key, lifetime)
	if err != nil {
		return "", s.presignErr(ctx, err)
	}
	return u, nil
}

func (s *Store) presignErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("%w: presign: %w", artifact.ErrStorage, err)
}

// TakeStaged reads a staged object, checks its size and digest, and deletes
// it. The object is deleted even when the check fails; a failed delete is left
// to Sweep.
func (s *Store) TakeStaged(ctx context.Context, name, digest string, size, limit int64) ([]byte, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	key, err := stagingKey(s.prefix, name)
	if err != nil {
		return nil, err
	}
	if size < 0 || size > limit {
		return nil, invalid("staged object size is out of range")
	}
	// One byte of slack lets a longer object be told from a read failure.
	raw, err := s.objects.Get(ctx, key, size+1)
	switch {
	case errors.Is(err, ErrObjectNotFound):
		return nil, artifact.ErrNotFound
	case err != nil && ctx.Err() != nil:
		return nil, ctx.Err()
	case err != nil:
		return nil, fmt.Errorf("%w: read staged object: %w", artifact.ErrStorage, err)
	}
	delCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_ = s.objects.Delete(delCtx, key)
	if int64(len(raw)) != size || digestOf(raw) != digest {
		return nil, fmt.Errorf("%w: staged object changed", artifact.ErrCorrupt)
	}
	return raw, nil
}

// sweepStaged deletes staged objects older than stagedMaxAge and returns how
// many it deleted. No row references them, so it needs no installation lock.
func (s *Store) sweepStaged(ctx context.Context) (int, error) {
	cutoff := time.Now().Add(-stagedMaxAge)
	var keys []string
	err := s.objects.List(ctx, stagingPrefix(s.prefix), func(o ObjectInfo) error {
		if o.Modified.Before(cutoff) {
			keys = append(keys, o.Key)
		}
		return nil
	})
	deleted := 0
	if err == nil {
		for _, k := range keys {
			if err = s.objects.Delete(ctx, k); err != nil {
				break
			}
			deleted++
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return deleted, ctx.Err()
		}
		return deleted, fmt.Errorf("%w: sweep staged: %w", artifact.ErrStorage, err)
	}
	return deleted, nil
}
