package pg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lineahq/linea/internal/debugger/artifact"
)

var _ artifact.BuildTransfers = (*Store)(nil)

// BuildInputURL presigns a read of one input of a build.
func (s *Store) BuildInputURL(ctx context.Context, id, digest string, lifetime time.Duration) (artifact.ObjectURL, error) {
	if err := s.ready(ctx); err != nil {
		return artifact.ObjectURL{}, err
	}
	if lifetime <= 0 {
		return artifact.ObjectURL{}, invalid("invalid build input URL request")
	}
	var size int64
	err := s.pool.QueryRow(ctx, "SELECT bytes FROM build_inputs WHERE build_id = $1 AND digest = $2 LIMIT 1", id, digest).Scan(&size)
	if errors.Is(err, pgx.ErrNoRows) {
		return artifact.ObjectURL{}, artifact.ErrNotFound
	}
	if err != nil {
		return artifact.ObjectURL{}, storageErr("build input", err)
	}
	key, err := objectKey(s.prefix, digest)
	if err != nil {
		return artifact.ObjectURL{}, err
	}
	u, err := s.objects.PresignGet(ctx, key, lifetime)
	if err != nil {
		return artifact.ObjectURL{}, s.presignErr(ctx, err)
	}
	return artifact.ObjectURL{URL: u, Digest: digest, Size: size}, nil
}

// holdsBuild checks that builder holds build id in attempt: an unknown build
// is ErrNotFound, one in another state or held by another builder ErrConflict.
func holdsBuild(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, id, builder string, attempt int, lock string) error {
	var state, holder string
	var held int
	err := q.QueryRow(ctx, "SELECT state, builder, attempt FROM builds WHERE id = $1"+lock, id).Scan(&state, &holder, &held)
	if errors.Is(err, pgx.ErrNoRows) {
		return artifact.ErrNotFound
	}
	if err != nil {
		return storageErr("build lease", err)
	}
	if state != string(artifact.BuildRunning) || holder != builder || held != attempt {
		return artifact.ErrConflict
	}
	return nil
}

// errRollback ends a transaction that only checks, without committing.
var errRollback = errors.New("rollback")

// StoreBuildBinary verifies the staged object name and stores it as the
// binary of build id, then records it on the running build, so the object is
// referenced (object_refs) until the build ends, as the build's binary if
// FinishBuild names it.
//
// No step that moves the binary holds the installation lock. The object is
// hashed by a streaming read, so a binary of hundreds of megabytes never sits
// in memory, and a short check of the lease and the storage limit, with the
// build's own reservation (Limits.BuildBytes) released, refuses early
// what the commit would refuse. The object is then copied inside object
// storage to its content-addressed key, conditionally on the ETag that was
// hashed, so a staged object rewritten after the check is never stored.
// Under the lock the commit checks the lease and the limit again, confirms
// that the copy is still there when no row references its digest, as Sweep
// deletes only under the lock, and records it. A copy the commit refuses is
// unreferenced and left to Sweep. The staged object is deleted in every case;
// a failed delete is left to Sweep too.
func (s *Store) StoreBuildBinary(ctx context.Context, id, builder string, attempt int, name, digest string, size, limit int64) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	stagedKey, err := stagingKey(s.prefix, name)
	if err != nil {
		return err
	}
	defer func() {
		delCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = s.objects.Delete(delCtx, stagedKey)
	}()
	blobKey, err := objectKey(s.prefix, digest)
	if err != nil {
		return invalid("invalid binary digest")
	}
	if size <= 0 || limit <= 0 {
		return invalid("invalid binary size")
	}
	if err := holdsBuild(ctx, s.pool, id, builder, attempt, ""); err != nil {
		return err
	}
	if size > min(limit, artifact.MaxRuntimeBinary) {
		return fmt.Errorf("%w: binary exceeds %d bytes", artifact.ErrLimit, min(limit, artifact.MaxRuntimeBinary))
	}
	etag, err := s.verifyStaged(ctx, stagedKey, digest, size)
	if err != nil {
		return err
	}
	// check runs the lease and limit checks; it reports whether no row
	// references digest.
	check := func(tx pgx.Tx) (bool, error) {
		if _, err := lockInstallation(ctx, tx); err != nil {
			return false, err
		}
		if err := holdsBuild(ctx, tx, id, builder, attempt, " FOR UPDATE"); err != nil {
			return false, err
		}
		// A second binary of the same build replaces the first, which stops
		// counting here.
		if _, err := tx.Exec(ctx, "UPDATE builds SET staged_binary_digest = '', staged_binary_bytes = 0 WHERE id = $1", id); err != nil {
			return false, err
		}
		additional, err := newBytes(ctx, tx, blob{digest, size})
		if err != nil {
			return false, err
		}
		return additional > 0, s.checkSpace(ctx, tx, additional, reserve{excludeBuild: id})
	}
	// The early check commits nothing: it rolls back once it has answered.
	if err := s.inTx(ctx, "check build binary", func(tx pgx.Tx) error {
		if _, err := check(tx); err != nil {
			return err
		}
		return errRollback
	}); err != nil && !errors.Is(err, errRollback) {
		return err
	}
	if err := s.objects.Copy(ctx, stagedKey, blobKey, etag); err != nil {
		switch {
		case errors.Is(err, ErrObjectNotFound):
			return artifact.ErrNotFound
		case errors.Is(err, ErrObjectChanged):
			return fmt.Errorf("%w: staged object changed", artifact.ErrCorrupt)
		case ctx.Err() != nil:
			return ctx.Err()
		}
		return fmt.Errorf("%w: store object: %w", artifact.ErrStorage, err)
	}
	return s.inTx(ctx, "store build binary", func(tx pgx.Tx) error {
		unreferenced, err := check(tx)
		if err != nil {
			return err
		}
		if unreferenced {
			// A sweep that listed an older object under this key before the
			// copy may have deleted it since.
			if err := s.blobPresent(ctx, blobKey, size); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, "UPDATE builds SET staged_binary_digest = $2, staged_binary_bytes = $3 WHERE id = $1", id, digest, size)
		return err
	})
}

// blobPresent confirms that key holds an object of size bytes without
// reading it. A missing object is ErrNotFound, another size ErrCorrupt.
func (s *Store) blobPresent(ctx context.Context, key string, size int64) error {
	r, err := s.objects.Open(ctx, key)
	switch {
	case errors.Is(err, ErrObjectNotFound):
		return fmt.Errorf("%w: stored binary is gone", artifact.ErrNotFound)
	case err != nil && ctx.Err() != nil:
		return ctx.Err()
	case err != nil:
		return fmt.Errorf("%w: check stored object: %w", artifact.ErrStorage, err)
	}
	r.Body.Close()
	if r.Size != size {
		return fmt.Errorf("%w: stored object has %d bytes, not %d", artifact.ErrCorrupt, r.Size, size)
	}
	return nil
}

// verifyStaged streams the object and checks its size and SHA-256, and
// returns the ETag of what it read. A missing object is ErrNotFound, any
// other size or digest ErrCorrupt.
func (s *Store) verifyStaged(ctx context.Context, key, digest string, size int64) (string, error) {
	r, err := s.objects.Open(ctx, key)
	switch {
	case errors.Is(err, ErrObjectNotFound):
		return "", artifact.ErrNotFound
	case err != nil && ctx.Err() != nil:
		return "", ctx.Err()
	case err != nil:
		return "", fmt.Errorf("%w: read staged object: %w", artifact.ErrStorage, err)
	}
	defer r.Body.Close()
	if r.Size != size {
		return "", fmt.Errorf("%w: staged object changed", artifact.ErrCorrupt)
	}
	h := sha256.New()
	// One byte beyond size tells a longer object from a read failure.
	n, err := io.Copy(h, io.LimitReader(r.Body, size+1))
	switch {
	case err != nil && ctx.Err() != nil:
		return "", ctx.Err()
	case err != nil:
		return "", fmt.Errorf("%w: read staged object: %w", artifact.ErrStorage, err)
	case n != size || "sha256:"+hex.EncodeToString(h.Sum(nil)) != digest:
		return "", fmt.Errorf("%w: staged object changed", artifact.ErrCorrupt)
	}
	return r.ETag, nil
}

// RuntimeBinaryURL presigns a read of a stored runtime binary. Only a digest
// a catalog entry or an unfinished run references qualifies, the objects a
// pool worker may be sent.
func (s *Store) RuntimeBinaryURL(ctx context.Context, digest string, lifetime time.Duration) (artifact.ObjectURL, error) {
	if err := s.ready(ctx); err != nil {
		return artifact.ObjectURL{}, err
	}
	if lifetime <= 0 || !artifact.ValidRuntimeIdentity(digest) {
		return artifact.ObjectURL{}, invalid("invalid runtime binary URL request")
	}
	var size int64
	err := s.pool.QueryRow(ctx, `SELECT binary_bytes FROM runtimes WHERE binary_digest = $1
		UNION ALL SELECT binary_bytes FROM runs WHERE binary_digest = $1 AND NOT terminal LIMIT 1`, digest).Scan(&size)
	if errors.Is(err, pgx.ErrNoRows) {
		return artifact.ObjectURL{}, artifact.ErrNotFound
	}
	if err != nil {
		return artifact.ObjectURL{}, storageErr("runtime binary", err)
	}
	key, err := objectKey(s.prefix, digest)
	if err != nil {
		return artifact.ObjectURL{}, err
	}
	u, err := s.objects.PresignGet(ctx, key, lifetime)
	if err != nil {
		return artifact.ObjectURL{}, s.presignErr(ctx, err)
	}
	return artifact.ObjectURL{URL: u, Digest: digest, Size: size}, nil
}
