package pg

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lineahq/linea/internal/debugger/artifact"
)

const sweepBatch = 500

// Sweep deletes the objects no row references and that are older than
// olderThan, and returns how many it deleted. It is safe to run on any
// instance, concurrently with writes: each batch is checked and deleted under
// the installation lock that every writer of objects holds from its upload to
// its commit, so an unreferenced object is never one a writer is about to
// reference. olderThan (the default is one day) is a margin on top of
// that. Sweep also deletes staged worker-transfer objects (transfers.go) older
// than one hour whatever olderThan is, and counts them; no row references
// them, so they need no lock. Housekeep runs it from one leader.
func (s *Store) Sweep(ctx context.Context, olderThan time.Duration) (int, error) {
	if err := s.ready(ctx); err != nil {
		return 0, err
	}
	deleted, err := s.sweepStaged(ctx)
	if err != nil {
		return deleted, err
	}
	cutoff := time.Now().Add(-olderThan)
	prefix := objectPrefix(s.prefix)
	type candidate struct{ key, digest string }
	var batch []candidate
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		digests := make([]string, len(batch))
		for i, c := range batch {
			digests[i] = c.digest
		}
		err := s.inTx(ctx, "sweep", func(tx pgx.Tx) error {
			if _, err := lockInstallation(ctx, tx); err != nil {
				return err
			}
			rows, err := tx.Query(ctx, "SELECT DISTINCT digest FROM object_refs WHERE digest = ANY($1)", digests)
			if err != nil {
				return err
			}
			referenced := map[string]bool{}
			for rows.Next() {
				var d string
				if err := rows.Scan(&d); err != nil {
					rows.Close()
					return err
				}
				referenced[d] = true
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			for _, c := range batch {
				if referenced[c.digest] {
					continue
				}
				if err := s.objects.Delete(ctx, c.key); err != nil {
					return fmt.Errorf("%w: sweep: %w", artifact.ErrStorage, err)
				}
				deleted++
			}
			return nil
		})
		batch = batch[:0]
		return err
	}
	err = s.objects.List(ctx, prefix, func(o ObjectInfo) error {
		name, ok := strings.CutPrefix(o.Key, prefix)
		if !ok || !isLowerHex(name, 64) || !o.Modified.Before(cutoff) {
			return nil
		}
		batch = append(batch, candidate{key: o.Key, digest: "sha256:" + name})
		if len(batch) >= sweepBatch {
			return flush()
		}
		return nil
	})
	if err == nil {
		err = flush()
	}
	if err != nil {
		if isRepositoryError(err) {
			return deleted, err
		}
		return deleted, fmt.Errorf("%w: sweep: %w", artifact.ErrStorage, err)
	}
	return deleted, nil
}
