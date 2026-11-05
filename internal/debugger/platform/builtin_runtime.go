package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/release"
	"github.com/lineahq/linea/internal/debugger/worker"
	"go.uber.org/zap"
)

// The built-in runtime on a team server: no Collector runs in the server,
// and nobody registers the built-in runtime. A worker that says it is the
// built-in runtime (worker.Hello.BuiltIn) of this server's release registers
// it, under a catalog name of this release that no request or registration
// can name. Instances of different releases, as in a rolling upgrade, each
// bind the built-in runtime to their own release's entry.

// builtInEntryPrefix starts the catalog name of a release's built-in
// runtime. '@' is not valid in a runtime name, so no registration, build or
// worker takes one.
const builtInEntryPrefix = BuiltInRuntime + "@"

// builtInEntryName is the catalog name of a release's built-in runtime.
func builtInEntryName(productVersion string) string { return builtInEntryPrefix + productVersion }

// isBuiltInEntry reports whether a catalog name is a release's built-in
// runtime.
func isBuiltInEntry(name string) bool { return strings.HasPrefix(name, builtInEntryPrefix) }

// ownBuiltInEntry is the catalog name of this release's built-in runtime.
func ownBuiltInEntry() string { return builtInEntryName(release.ProductVersion) }

// selects reports whether a request's runtime field names a runtime that
// runs outside this process: a built one, or on a team server the built-in
// one.
func (s *Service) selects(name string) bool {
	return selectsRuntime(name) || name == BuiltInRuntime && s.builtInOnWorkers
}

// registerBuiltIn records a connected worker as this release's built-in
// runtime when its hello says it is the built-in runtime of this server's
// release. It writes only when the entry is missing or names another
// identity, and then removes the entries of other releases no connected
// worker serves.
func (s *Service) registerBuiltIn(h worker.Hello) {
	if !s.builtInOnWorkers || !h.BuiltIn || h.ProductVersion != release.ProductVersion || h.Pool != nil && !h.Pool.Own {
		return
	}
	name := ownBuiltInEntry()
	ctx := s.ctx
	e, err := s.catalog.Runtime(ctx, name)
	switch {
	case err == nil && e.Identity == h.Runtime:
		return
	case err != nil && !errors.Is(err, artifact.ErrNotFound):
		s.logError("read the built-in runtime's catalog entry", err)
		return
	}
	// The catalog keeps what describes the build, not how one worker or
	// pool runs it.
	h.Name, h.Capacity, h.Pool = "", 0, nil
	raw, err := json.Marshal(h)
	if err != nil {
		s.logError("record the built-in runtime", err)
		return
	}
	now := s.now().UTC()
	if err := s.catalog.SaveRuntime(ctx, artifact.RuntimeEntry{Name: name, Identity: h.Runtime, Hello: raw,
		RegisteredBy: "linea", Registered: now, Confirmed: now}); err != nil {
		s.logError("record the built-in runtime", err)
		return
	}
	if s.logger != nil {
		s.logger.Info("built-in runtime registered", zap.String("release", release.ProductVersion), zap.String("runtime", h.Runtime))
	}
	s.pruneBuiltIn(ctx, name)
}

// pruneBuiltIn removes the built-in entries of other releases that no
// connected worker serves; a release still connected, as during a rolling
// upgrade, keeps its entry.
func (s *Service) pruneBuiltIn(ctx context.Context, own string) {
	entries, err := s.catalog.RuntimeEntries(ctx)
	if err != nil {
		s.logError("list the built-in runtimes of other releases", err)
		return
	}
	counts, err := s.workerCounts(ctx)
	if err != nil {
		s.logError("list the built-in runtimes of other releases", err)
		return
	}
	for _, e := range entries {
		if !isBuiltInEntry(e.Name) || e.Name == own || counts[e.Identity].Workers > 0 {
			continue
		}
		if err := s.catalog.DeleteRuntime(ctx, e.Name); err != nil && !errors.Is(err, artifact.ErrNotFound) {
			s.logError("remove the built-in runtime of another release", err)
		}
	}
}

// builtInEntry returns this release's built-in entry and its hello; a
// release no worker has registered yet is unavailable.
func (s *Service) builtInEntry(ctx context.Context) (artifact.RuntimeEntry, worker.Hello, error) {
	e, err := s.catalog.Runtime(ctx, ownBuiltInEntry())
	if errors.Is(err, artifact.ErrNotFound) {
		return e, worker.Hello{}, &Error{Code: CodeUnavailable, Message: fmt.Sprintf(
			"the built-in runtime (Linea %s, Collector %s) is not available: no worker of this release has connected yet; start linea-worker --connect from this release's image",
			release.ProductVersion, release.CollectorVersion), cause: err}
	}
	if err != nil {
		return e, worker.Hello{}, classify(err, CodeStorageError)
	}
	var h worker.Hello
	if err := json.Unmarshal(e.Hello, &h); err != nil {
		return e, h, classify(fmt.Errorf("%w: runtime %s: %w", artifact.ErrCorrupt, e.Name, err), CodeStorageError)
	}
	return e, h, nil
}

// bindBuiltIn binds the built-in runtime to the connected workers of this
// release, whether or not one is connected now.
func (s *Service) bindBuiltIn(ctx context.Context) (*boundRuntime, error) {
	_, h, err := s.builtInEntry(ctx)
	if err != nil {
		return nil, err
	}
	return pooledRuntime(s, BuiltInRuntime, h, nil), nil
}

// builtInInfo describes this release's built-in runtime as the catalog
// lists it, under the name requests select.
func (s *Service) builtInInfo(ctx context.Context) (*RuntimeInfo, error) {
	e, h, err := s.builtInEntry(ctx)
	if err != nil {
		return nil, err
	}
	counts, err := s.workerCounts(ctx)
	if err != nil {
		return nil, err
	}
	info := catalogInfo(e, h, counts[e.Identity])
	info.Name = BuiltInRuntime
	return info, nil
}
