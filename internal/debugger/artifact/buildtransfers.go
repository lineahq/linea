package artifact

import (
	"context"
	"time"
)

// MaxRuntimeBinary bounds a built runtime binary.
const MaxRuntimeBinary = 512 << 20

// BuildAllowance is the most a team server reserves for each queued or
// running build's binary (Limits.BuildBytes): an eighth of its storage limit,
// at most this.
const BuildAllowance int64 = 128 << 20

// BuildTransfers moves a build's inputs to its builder and the built binary
// back through object storage, so builder frames stay small.
// A repository with Builds and Transfers implements it; builders connect
// only to a repository that does.
type BuildTransfers interface {
	// BuildInputURL returns a URL that reads the input of build id with
	// digest without credentials until lifetime ends, with its digest and
	// size. A digest that is not one of the build's inputs, or an unknown
	// build, is ErrNotFound.
	BuildInputURL(ctx context.Context, id, digest string, lifetime time.Duration) (ObjectURL, error)
	// StoreBuildBinary moves the staged object name (Transfers.StageURL)
	// into storage as the binary of build id, which builder holds in
	// attempt. It reads the object to check its size and digest and deletes
	// it in every case. A lease the builder no longer holds is ErrConflict,
	// another size or digest ErrCorrupt, a missing object ErrNotFound, and
	// a size beyond limit or the storage limit ErrLimit. The stored binary
	// stays while that lease is held; FinishBuild then references it.
	StoreBuildBinary(ctx context.Context, id, builder string, attempt int, name, digest string, size, limit int64) error
	// RuntimeBinaryURL returns a URL that reads the stored runtime binary
	// with digest without credentials until lifetime ends, for a pool
	// worker. A digest no catalog entry or unfinished run
	// references is ErrNotFound.
	RuntimeBinaryURL(ctx context.Context, digest string, lifetime time.Duration) (ObjectURL, error)
}
