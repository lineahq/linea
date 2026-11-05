package artifact

import (
	"context"
	"time"
)

// Transfers moves payloads between the repository and workers through
// object storage, so that worker frames stay small. A
// repository without object storage does not implement it, and its workers
// receive and return payloads inline.
type Transfers interface {
	// RunBundleURL returns a URL that reads a run's private input bundle
	// without credentials until lifetime ends, with the bundle's digest and
	// size. A run without a bundle, or an unknown one, is ErrNotFound.
	RunBundleURL(ctx context.Context, id string, lifetime time.Duration) (ObjectURL, error)
	// StageURL returns a URL that writes one staged object, named by the
	// caller, until lifetime ends. Names are lower-case letters, digits and
	// '-', at most 128 characters.
	StageURL(ctx context.Context, name string, lifetime time.Duration) (string, error)
	// TakeStaged reads a staged object and deletes it. An object of another
	// size or digest is ErrCorrupt; a missing one is ErrNotFound. size must
	// not exceed limit.
	TakeStaged(ctx context.Context, name, digest string, size, limit int64) ([]byte, error)
}

// ObjectURL is a presigned URL with the digest ("sha256:<hex>") and size of
// the object it reads.
type ObjectURL struct {
	URL    string
	Digest string
	Size   int64
}
