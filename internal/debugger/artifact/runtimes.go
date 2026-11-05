package artifact

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
)

// MaxRuntimeEntries bounds the runtime catalog, MaxRuntimeHello one
// entry's recorded hello and MaxRuntimeManifest its manifest.
const (
	MaxRuntimeEntries  = 256
	MaxRuntimeHello    = 1 << 20
	MaxRuntimeManifest = 1 << 20
)

// RuntimeEntry is a runtime catalog entry: a name for the
// worker build whose hello reports Identity ("sha256:<hex>"). Image and
// Digest say where the build comes from, or Binary and Build for a runtime
// built on the server; Hello is the registering worker's
// hello, encoded. Manifest is the OCB manifest the runtime was built from,
// as sent, when it is known (empty otherwise). Confirmed is zero until a
// connected worker of Identity said hello.
type RuntimeEntry struct {
	Name         string
	Identity     string
	Image        string
	Digest       string
	Binary       *RuntimeBinary
	Build        string
	Hello        []byte
	Manifest     []byte
	RegisteredBy string
	Registered   time.Time
	Confirmed    time.Time
	// Suspect is set once a pool worker found Binary wrong (MarkSuspect);
	// SaveRuntime always records an entry without it.
	Suspect *Suspicion
}

// Suspicion is why a pool worker distrusts a runtime binary:
// the failure it reported, the pool's presence identity and when.
type Suspicion struct {
	Cause string
	Pool  string
	At    time.Time
}

// MaxSuspicionCause bounds a suspicion's recorded cause, in bytes.
const MaxSuspicionCause = 512

// ValidateSuspicion reports why a repository refuses m, as
// ErrInvalidRequest: a printable cause of 1 to MaxSuspicionCause bytes, a
// pool presence identity and a time.
func ValidateSuspicion(m Suspicion) error {
	switch {
	case m.Cause == "" || len(m.Cause) > MaxSuspicionCause || !printable(m.Cause):
		return fmt.Errorf("%w: a suspicion needs a printable cause of 1 to %d bytes", ErrInvalidRequest, MaxSuspicionCause)
	case !validPoolIdentity(m.Pool):
		return fmt.Errorf("%w: a suspicion names its pool as pool:<os>/<arch>", ErrInvalidRequest)
	case m.At.IsZero():
		return fmt.Errorf("%w: a suspicion needs a time", ErrInvalidRequest)
	}
	return nil
}

// RuntimeBinary is a stored worker binary: its digest ("sha256:<hex>"),
// size and the platform it runs on.
type RuntimeBinary struct {
	Digest string
	Bytes  int64
	OS     string
	Arch   string
}

// Runtimes is the runtime catalog, by name. Several names may share an
// identity.
type Runtimes interface {
	// SaveRuntime records an entry, replacing the one of the same name. A
	// new name beyond MaxRuntimeEntries is ErrLimit.
	SaveRuntime(ctx context.Context, e RuntimeEntry) error
	// Runtime returns name's entry, or ErrNotFound.
	Runtime(ctx context.Context, name string) (RuntimeEntry, error)
	// RuntimeEntries returns every entry, by name.
	RuntimeEntries(ctx context.Context) ([]RuntimeEntry, error)
	// ConfirmRuntimes sets Confirmed to at on every unconfirmed entry of
	// identity.
	ConfirmRuntimes(ctx context.Context, identity string, at time.Time) error
	// DeleteRuntime removes name's entry, or is ErrNotFound. The entry the
	// default runtime names is ErrConflict.
	DeleteRuntime(ctx context.Context, name string) error
	// MarkSuspect sets m on every entry whose binary has digest and has no
	// suspicion yet; the first suspicion stays. It reports how many entries
	// it marked. Saving an entry again clears its mark.
	MarkSuspect(ctx context.Context, digest string, m Suspicion) (int, error)
	// DefaultRuntime returns the default runtime, or ErrNotFound while
	// none was set.
	DefaultRuntime(ctx context.Context) (DefaultRuntime, error)
	// SetDefaultRuntime records d as the default runtime and returns the
	// one it replaced, zero when none was set. With entry, d.Name must name
	// an entry, or it is ErrNotFound; that check and DeleteRuntime's
	// refusal of the default exclude each other. Every write records a
	// ChangeDefaultRuntime.
	SetDefaultRuntime(ctx context.Context, d DefaultRuntime, entry bool) (DefaultRuntime, error)
}

// DefaultRuntime is the team's default runtime: the runtime
// of every request that names none, who chose it and when.
type DefaultRuntime struct {
	Name  string
	SetBy string
	Set   time.Time
}

// ValidateDefaultRuntime reports why a repository refuses d, as
// ErrInvalidRequest: a name of 1 to 128 printable characters, a printable
// chooser of at most 256 characters and a time.
func ValidateDefaultRuntime(d DefaultRuntime) error {
	switch {
	case d.Name == "" || len(d.Name) > 128 || !printable(d.Name):
		return fmt.Errorf("%w: default runtime name must be 1 to 128 printable characters", ErrInvalidRequest)
	case len(d.SetBy) > 256 || !printable(d.SetBy):
		return fmt.Errorf("%w: default runtime chooser too long or not printable", ErrInvalidRequest)
	case d.Set.IsZero():
		return fmt.Errorf("%w: default runtime needs a time", ErrInvalidRequest)
	}
	return nil
}

// ValidateRuntimeEntry reports why a repository refuses e, as
// ErrInvalidRequest: a name of 1 to 128 printable characters, a SHA-256
// identity, printable image and digest of at most 512 characters, a hello
// of at most MaxRuntimeHello bytes, a manifest of at most MaxRuntimeManifest
// bytes and a registration time.
func ValidateRuntimeEntry(e RuntimeEntry) error {
	switch {
	case e.Name == "" || len(e.Name) > 128 || !printable(e.Name):
		return fmt.Errorf("%w: runtime name must be 1 to 128 printable characters", ErrInvalidRequest)
	case !ValidRuntimeIdentity(e.Identity):
		return fmt.Errorf("%w: runtime identity must be sha256:<64 lowercase hex digits>", ErrInvalidRequest)
	case len(e.Image) > 512 || len(e.Digest) > 512 || len(e.RegisteredBy) > 256 || !printable(e.Image+e.Digest+e.RegisteredBy):
		return fmt.Errorf("%w: runtime image, digest or registrant too long or not printable", ErrInvalidRequest)
	case e.Binary != nil && (!ValidRuntimeIdentity(e.Binary.Digest) || e.Binary.Bytes <= 0 || e.Binary.Bytes > MaxRuntimeBinary ||
		!platformName(e.Binary.OS) || !platformName(e.Binary.Arch)):
		return fmt.Errorf("%w: runtime binary needs a sha256 digest, a size of at most %d bytes, an OS and an architecture", ErrInvalidRequest, MaxRuntimeBinary)
	case len(e.Build) > 128 || !printable(e.Build) || (e.Build != "") != (e.Binary != nil):
		return fmt.Errorf("%w: exactly a runtime with a binary names its build", ErrInvalidRequest)
	case len(e.Hello) > MaxRuntimeHello:
		return fmt.Errorf("%w: runtime hello exceeds %d bytes", ErrInvalidRequest, MaxRuntimeHello)
	case len(e.Manifest) > MaxRuntimeManifest:
		return fmt.Errorf("%w: runtime manifest exceeds %d bytes", ErrInvalidRequest, MaxRuntimeManifest)
	case e.Registered.IsZero():
		return fmt.Errorf("%w: runtime entry needs a registration time", ErrInvalidRequest)
	}
	return nil
}

// ValidRuntimeIdentity reports whether id is "sha256:" and 64 lowercase hex
// digits.
func ValidRuntimeIdentity(id string) bool {
	h, ok := strings.CutPrefix(id, "sha256:")
	if !ok || len(h) != 64 {
		return false
	}
	return !strings.ContainsFunc(h, func(r rune) bool { return (r < '0' || r > '9') && (r < 'a' || r > 'f') })
}

func printable(s string) bool { return !strings.ContainsFunc(s, unicode.IsControl) }

// platformName reports whether s is a GOOS or GOARCH value: 1 to 32
// lowercase letters and digits.
func platformName(s string) bool {
	return s != "" && len(s) <= 32 && !strings.ContainsFunc(s, func(r rune) bool { return (r < 'a' || r > 'z') && (r < '0' || r > '9') })
}

// MaxPresentWorkers bounds the workers one instance records as connected.
const MaxPresentWorkers = 4096

// MaxPresentWarm bounds the runtimes one pool worker records as running.
const MaxPresentWarm = 64

// PresentWorker is a worker connected to an instance: its ID in the queue, the
// runtime identity its hello reported, or PoolIdentity of its platform for a
// pool worker, and its capacity.
type PresentWorker struct {
	ID       string
	Identity string
	Capacity int
	// Warm lists the runtime identities a pool worker holds running; it is
	// empty for any other worker.
	Warm []string
	// Own is the runtime identity a pool worker runs from its own build, under
	// which it counts as a worker too; empty for any other worker.
	Own string
}

// PoolIdentity is the identity the pool workers of platform ("os/arch") are
// recorded under, which no runtime identity ("sha256:<hex>") can be.
func PoolIdentity(platform string) string { return "pool:" + platform }

// validPoolIdentity reports whether id is PoolIdentity of a valid platform.
func validPoolIdentity(id string) bool {
	p, ok := strings.CutPrefix(id, "pool:")
	os, arch, cut := strings.Cut(p, "/")
	return ok && cut && platformName(os) && platformName(arch)
}

// WorkerCount counts the connected workers of one runtime identity and
// their slots.
type WorkerCount struct {
	Workers int
	Slots   int
	// OwnWorkers and OwnSlots are the part of them counted for pools that
	// run the identity as their own build (PresentWorker.Own).
	OwnWorkers int
	OwnSlots   int
}

// Add counts one more worker of capacity slots, own when it is a pool
// counted for its own build.
func (c WorkerCount) Add(capacity int, own bool) WorkerCount {
	c.Workers++
	c.Slots += capacity
	if own {
		c.OwnWorkers++
		c.OwnSlots += capacity
	}
	return c
}

// Presence records the workers connected to each instance, so
// every instance counts the workers of all of them. An instance renews its
// record while it runs; the record of one that stopped expires. Times come
// from the caller, as the queue's do.
type Presence interface {
	// SetWorkers replaces the instance's record with workers, valid until
	// expires. No workers removes the record.
	SetWorkers(ctx context.Context, instance string, workers []PresentWorker, expires time.Time) error
	// WorkerCounts counts the workers of records that have not expired at
	// now, by runtime identity; a pool counts under its platform and under
	// its Own runtime.
	WorkerCounts(ctx context.Context, now time.Time) (map[string]WorkerCount, error)
	// WarmRuntimes returns the runtime identities that pool workers hold
	// running in the records of instances other than except that have not
	// expired at now.
	WarmRuntimes(ctx context.Context, now time.Time, except string) (map[string]bool, error)
}

// ValidatePresence reports why a repository refuses an instance's record.
func ValidatePresence(instance string, workers []PresentWorker) error {
	if instance == "" || len(instance) > 128 || !printable(instance) {
		return fmt.Errorf("%w: instance must be 1 to 128 printable characters", ErrInvalidRequest)
	}
	if len(workers) > MaxPresentWorkers {
		return fmt.Errorf("%w: at most %d workers per instance", ErrLimit, MaxPresentWorkers)
	}
	for _, w := range workers {
		switch {
		case w.ID == "" || len(w.ID) > 128 || !printable(w.ID):
			return fmt.Errorf("%w: worker ID must be 1 to 128 printable characters", ErrInvalidRequest)
		case !ValidRuntimeIdentity(w.Identity) && !validPoolIdentity(w.Identity):
			return fmt.Errorf("%w: worker %s names no runtime identity", ErrInvalidRequest, w.ID)
		case w.Capacity < 1:
			return fmt.Errorf("%w: worker %s has no capacity", ErrInvalidRequest, w.ID)
		case len(w.Warm) > 0 && !validPoolIdentity(w.Identity):
			return fmt.Errorf("%w: worker %s is no pool and holds no runtimes warm", ErrInvalidRequest, w.ID)
		case len(w.Warm) > MaxPresentWarm:
			return fmt.Errorf("%w: at most %d warm runtimes per pool", ErrLimit, MaxPresentWarm)
		case slices.ContainsFunc(w.Warm, func(id string) bool { return !ValidRuntimeIdentity(id) }):
			return fmt.Errorf("%w: worker %s holds a warm runtime with no runtime identity", ErrInvalidRequest, w.ID)
		case w.Own != "" && (!validPoolIdentity(w.Identity) || !ValidRuntimeIdentity(w.Own)):
			return fmt.Errorf("%w: worker %s runs its own build only as a pool with a runtime identity", ErrInvalidRequest, w.ID)
		}
	}
	return nil
}
