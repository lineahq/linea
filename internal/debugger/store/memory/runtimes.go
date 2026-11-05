package memory

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
)

// runtimeCatalog is the runtime catalog logic both Store and Runtimes share;
// callers hold the lock that guards it.
type runtimeCatalog map[string]artifact.RuntimeEntry

func cloneRuntime(e artifact.RuntimeEntry) artifact.RuntimeEntry {
	e.Hello = cloneBytes(e.Hello)
	e.Manifest = cloneBytes(e.Manifest)
	if e.Binary != nil {
		b := *e.Binary
		e.Binary = &b
	}
	if e.Suspect != nil {
		m := *e.Suspect
		m.At = m.At.UTC()
		e.Suspect = &m
	}
	e.Registered, e.Confirmed = e.Registered.UTC(), e.Confirmed.UTC()
	return e
}

func (c runtimeCatalog) save(e artifact.RuntimeEntry) error {
	if err := artifact.ValidateRuntimeEntry(e); err != nil {
		return err
	}
	if _, ok := c[e.Name]; !ok && len(c) >= artifact.MaxRuntimeEntries {
		return fmt.Errorf("%w: at most %d runtimes", artifact.ErrLimit, artifact.MaxRuntimeEntries)
	}
	e.Suspect = nil
	c[e.Name] = cloneRuntime(e)
	return nil
}

func (c runtimeCatalog) markSuspect(digest string, m artifact.Suspicion) (int, error) {
	if err := artifact.ValidateSuspicion(m); err != nil {
		return 0, err
	}
	n := 0
	for name, e := range c {
		if e.Binary != nil && e.Binary.Digest == digest && e.Suspect == nil {
			e.Suspect = &m
			c[name] = cloneRuntime(e)
			n++
		}
	}
	return n, nil
}

func (c runtimeCatalog) get(name string) (artifact.RuntimeEntry, error) {
	e, ok := c[name]
	if !ok {
		return artifact.RuntimeEntry{}, artifact.ErrNotFound
	}
	return cloneRuntime(e), nil
}

func (c runtimeCatalog) list() []artifact.RuntimeEntry {
	out := []artifact.RuntimeEntry{}
	for _, e := range c {
		out = append(out, cloneRuntime(e))
	}
	slices.SortFunc(out, func(a, b artifact.RuntimeEntry) int { return strings.Compare(a.Name, b.Name) })
	return out
}

func (c runtimeCatalog) confirm(identity string, at time.Time) {
	for name, e := range c {
		if e.Identity == identity && e.Confirmed.IsZero() {
			e.Confirmed = at.UTC()
			c[name] = e
		}
	}
}

func (c runtimeCatalog) remove(name string, def *artifact.DefaultRuntime) error {
	if _, ok := c[name]; !ok {
		return artifact.ErrNotFound
	}
	if def != nil && def.Name == name {
		return fmt.Errorf("%w: runtime %s is the default runtime", artifact.ErrConflict, name)
	}
	delete(c, name)
	return nil
}

// setDefault replaces *def with d and returns the one it replaced.
func (c runtimeCatalog) setDefault(def **artifact.DefaultRuntime, d artifact.DefaultRuntime, entry bool) (artifact.DefaultRuntime, error) {
	if err := artifact.ValidateDefaultRuntime(d); err != nil {
		return artifact.DefaultRuntime{}, err
	}
	if _, ok := c[d.Name]; entry && !ok {
		return artifact.DefaultRuntime{}, fmt.Errorf("%w: runtime %s", artifact.ErrNotFound, d.Name)
	}
	var previous artifact.DefaultRuntime
	if *def != nil {
		previous = **def
	}
	d.Set = d.Set.UTC()
	*def = &d
	return previous, nil
}

func getDefault(def *artifact.DefaultRuntime) (artifact.DefaultRuntime, error) {
	if def == nil {
		return artifact.DefaultRuntime{}, artifact.ErrNotFound
	}
	return *def, nil
}

// SaveRuntime records an entry, replacing the one of the same name.
func (s *Store) SaveRuntime(ctx context.Context, e artifact.RuntimeEntry) error {
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return err
	}
	return s.d.runtimes.save(e)
}

// Runtime returns name's entry.
func (s *Store) Runtime(ctx context.Context, name string) (artifact.RuntimeEntry, error) {
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return artifact.RuntimeEntry{}, err
	}
	return s.d.runtimes.get(name)
}

// RuntimeEntries returns every entry, by name.
func (s *Store) RuntimeEntries(ctx context.Context) ([]artifact.RuntimeEntry, error) {
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return nil, err
	}
	return s.d.runtimes.list(), nil
}

// ConfirmRuntimes confirms every unconfirmed entry of identity at at.
func (s *Store) ConfirmRuntimes(ctx context.Context, identity string, at time.Time) error {
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return err
	}
	s.d.runtimes.confirm(identity, at)
	return nil
}

// DeleteRuntime removes name's entry.
func (s *Store) DeleteRuntime(ctx context.Context, name string) error {
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return err
	}
	return s.d.runtimes.remove(name, s.d.defaultRuntime)
}

// DefaultRuntime returns the default runtime.
func (s *Store) DefaultRuntime(ctx context.Context) (artifact.DefaultRuntime, error) {
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return artifact.DefaultRuntime{}, err
	}
	return getDefault(s.d.defaultRuntime)
}

// SetDefaultRuntime records the default runtime.
func (s *Store) SetDefaultRuntime(ctx context.Context, d artifact.DefaultRuntime, entry bool) (artifact.DefaultRuntime, error) {
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return artifact.DefaultRuntime{}, err
	}
	previous, err := s.d.runtimes.setDefault(&s.d.defaultRuntime, d, entry)
	if err == nil {
		s.d.change(artifact.ChangeDefaultRuntime, "default")
	}
	return previous, err
}

// MarkSuspect marks every unmarked entry whose binary has digest.
func (s *Store) MarkSuspect(ctx context.Context, digest string, m artifact.Suspicion) (int, error) {
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return 0, err
	}
	return s.d.runtimes.markSuspect(digest, m)
}

// Runtimes is a runtime catalog on its own, for a service without a
// repository.
type Runtimes struct {
	mu  sync.Mutex
	c   runtimeCatalog
	def *artifact.DefaultRuntime
}

var _ artifact.Runtimes = (*Runtimes)(nil)

func NewRuntimes() *Runtimes { return &Runtimes{c: runtimeCatalog{}} }

func (r *Runtimes) SaveRuntime(ctx context.Context, e artifact.RuntimeEntry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.c.save(e)
}

func (r *Runtimes) Runtime(ctx context.Context, name string) (artifact.RuntimeEntry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.c.get(name)
}

func (r *Runtimes) RuntimeEntries(ctx context.Context) ([]artifact.RuntimeEntry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.c.list(), nil
}

func (r *Runtimes) ConfirmRuntimes(ctx context.Context, identity string, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.c.confirm(identity, at)
	return nil
}

func (r *Runtimes) DeleteRuntime(ctx context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.c.remove(name, r.def)
}

func (r *Runtimes) DefaultRuntime(ctx context.Context) (artifact.DefaultRuntime, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return getDefault(r.def)
}

func (r *Runtimes) SetDefaultRuntime(ctx context.Context, d artifact.DefaultRuntime, entry bool) (artifact.DefaultRuntime, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.c.setDefault(&r.def, d, entry)
}

func (r *Runtimes) MarkSuspect(ctx context.Context, digest string, m artifact.Suspicion) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.c.markSuspect(digest, m)
}
