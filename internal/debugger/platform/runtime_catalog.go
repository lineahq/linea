package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/authz"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/worker"
)

// The runtime catalog names the worker builds a request may
// select. `linea runtime register` starts an image's worker where Docker
// runs, reads its hello and registers the name with the hello's runtime
// identity; the server needs no Docker. A connected worker whose hello
// reports the identity confirms the entry. A run on a catalog runtime
// queues until a worker of its identity takes it; an inspection or preview
// needs one connected. A name in the catalog wins over a built runtime of
// the same name.

// RegisterRuntimeRequest registers a runtime: its name, the image it was
// read from and the hello of that image's worker.
type RegisterRuntimeRequest struct {
	Name string
	// Image is the image reference the hello was read from, and Digest its
	// registry digest ("sha256:<hex>") when known.
	Image  string
	Digest string
	// Hello is the worker's hello as the worker protocol encodes it.
	Hello json.RawMessage
}

// RuntimeImage is what ReadRuntimeImage read from an image: the request
// that registers it, without a name.
type RuntimeImage = RegisterRuntimeRequest

// helloReader is a runtime store that can start an image's worker only to
// read its hello.
type helloReader interface {
	ReadHello(ctx context.Context, ref string, pull bool) (digest string, hello []byte, err error)
}

// ReadRuntimeImage starts ref's worker where this service runs Docker,
// pulled first with pull, and returns its hello and digest for
// RegisterRuntime on a server, which needs no Docker. It runs no request.
func (s *Service) ReadRuntimeImage(ctx context.Context, caller Caller, ref string, pull bool) (_ *RuntimeImage, err error) {
	defer s.audit(&caller, "ReadRuntimeImage", &err)()
	if err := s.begin(ctx, caller, authz.RuntimeManage); err != nil {
		return nil, err
	}
	if strings.TrimSpace(ref) == "" {
		return nil, &Error{Code: CodeInvalidRequest, Message: "register needs an image reference"}
	}
	store, err := s.runtimeStore()
	if err != nil {
		return nil, err
	}
	r, ok := store.(helloReader)
	if !ok {
		return nil, errNoRuntimeStore()
	}
	digest, hello, err := r.ReadHello(ctx, ref, pull)
	if errors.Is(err, ErrRuntimeNotFound) {
		return nil, &Error{Code: CodeNotFound, Message: fmt.Sprintf("image %s not found; pull it or name one in Docker's store", ref), cause: err}
	}
	if err != nil {
		return nil, classify(err, CodeInvalidRequest)
	}
	return &RuntimeImage{Image: ref, Digest: digest, Hello: hello}, nil
}

// RegisterRuntime records a catalog entry, replacing the one of the same
// name. The hello is what the registrant read; only a connected worker's
// hello confirms it.
func (s *Service) RegisterRuntime(ctx context.Context, caller Caller, req RegisterRuntimeRequest) (_ *RuntimeInfo, err error) {
	defer s.audit(&caller, "RegisterRuntime", &err)()
	if err := s.begin(ctx, caller, authz.RuntimeManage); err != nil {
		return nil, err
	}
	var hello worker.Hello
	if err := json.Unmarshal(req.Hello, &hello); err != nil {
		return nil, &Error{Code: CodeInvalidRequest, Message: "the hello is not a worker hello: " + err.Error()}
	}
	switch {
	case req.Name == BuiltInRuntime || !validRuntimeName(req.Name):
		return nil, &Error{Code: CodeInvalidRequest, Message: fmt.Sprintf("runtime name %q is invalid: use 1 to 63 lowercase letters, digits and dashes, not %s", req.Name, BuiltInRuntime)}
	case !artifact.ValidRuntimeIdentity(hello.Runtime):
		return nil, &Error{Code: CodeInvalidRequest, Message: "the hello names no runtime identity (sha256:<hex>)"}
	case hello.Protocol != worker.ProtocolVersion:
		return nil, &Error{Code: CodeUnsupportedBuild, Message: fmt.Sprintf(
			"the image's worker speaks protocol %d, this debugger speaks %d: build it again with this debugger", hello.Protocol, worker.ProtocolVersion)}
	case strings.TrimSpace(req.Image) == "":
		return nil, &Error{Code: CodeInvalidRequest, Message: "register needs the image the hello was read from"}
	case req.Digest != "" && !artifact.ValidRuntimeIdentity(req.Digest):
		return nil, &Error{Code: CodeInvalidRequest, Message: "the digest is not sha256:<hex>"}
	}
	// The catalog keeps what describes the build, not how one worker runs it.
	hello.Name, hello.Capacity = "", 0
	raw, err := json.Marshal(hello)
	if err != nil {
		return nil, classify(err, CodeInternal)
	}
	e := artifact.RuntimeEntry{Name: req.Name, Identity: hello.Runtime, Image: req.Image, Digest: req.Digest, Hello: raw,
		RegisteredBy: caller.Actor().Principal, Registered: s.now().UTC()}
	counts, err := s.workerCounts(ctx)
	if err != nil {
		return nil, err
	}
	if counts[e.Identity].Workers > 0 {
		e.Confirmed = e.Registered
	}
	if err := s.catalog.SaveRuntime(ctx, e); err != nil {
		return nil, classify(err, CodeInvalidRequest)
	}
	return catalogInfo(e, hello, entryCount(e, counts)), nil
}

// catalogEntry returns name's catalog entry and its decoded hello; ok is
// false when the catalog has no such name. A release's built-in entry is
// never one a request names (builtInEntry).
func (s *Service) catalogEntry(ctx context.Context, name string) (artifact.RuntimeEntry, worker.Hello, bool, error) {
	if isBuiltInEntry(name) {
		return artifact.RuntimeEntry{}, worker.Hello{}, false, nil
	}
	e, err := s.catalog.Runtime(ctx, name)
	if errors.Is(err, artifact.ErrNotFound) {
		return e, worker.Hello{}, false, nil
	}
	if err != nil {
		return e, worker.Hello{}, false, classify(err, CodeStorageError)
	}
	var h worker.Hello
	if err := json.Unmarshal(e.Hello, &h); err != nil {
		return e, h, false, classify(fmt.Errorf("%w: runtime %s: %w", artifact.ErrCorrupt, name, err), CodeStorageError)
	}
	return e, h, true, nil
}

// catalogRuntimes lists the catalog as runtimes, by name.
func (s *Service) catalogRuntimes(ctx context.Context) ([]RuntimeInfo, error) {
	entries, err := s.catalog.RuntimeEntries(ctx)
	if err != nil {
		return nil, classify(err, CodeStorageError)
	}
	counts, err := s.workerCounts(ctx)
	if err != nil {
		return nil, err
	}
	own := ownBuiltInEntry()
	out := make([]RuntimeInfo, 0, len(entries))
	for _, e := range entries {
		// The built-in runtime is listed under its name, only for this
		// instance's release.
		if isBuiltInEntry(e.Name) && (!s.builtInOnWorkers || e.Name != own) {
			continue
		}
		var h worker.Hello
		if err := json.Unmarshal(e.Hello, &h); err != nil {
			return nil, classify(fmt.Errorf("%w: runtime %s: %w", artifact.ErrCorrupt, e.Name, err), CodeStorageError)
		}
		info := catalogInfo(e, h, entryCount(e, counts))
		s.buildManifest(ctx, info, e)
		if isBuiltInEntry(e.Name) {
			info.Name = BuiltInRuntime
		}
		out = append(out, *info)
	}
	return out, nil
}

// entryCount counts the connected workers that run a catalog entry: those
// of its identity and, for a runtime built on the server, the pool workers
// of its binary's platform, each pool once. A pool runs its own build only
// for requests that name no binary, so for such a runtime it counts by
// platform, not as its own build.
func entryCount(e artifact.RuntimeEntry, counts map[string]artifact.WorkerCount) artifact.WorkerCount {
	c := counts[e.Identity]
	if b := e.Binary; b != nil {
		p := counts[poolPresence(b.OS+"/"+b.Arch)]
		c.Workers += p.Workers - c.OwnWorkers
		c.Slots += p.Slots - c.OwnSlots
		c.OwnWorkers, c.OwnSlots = 0, 0
	}
	return c
}

// buildManifest fills the manifest of an entry built on the server before
// entries kept their manifest, from its build while the build is kept. It
// never writes the entry and leaves the manifest empty when the build is gone
// or unreadable.
func (s *Service) buildManifest(ctx context.Context, info *RuntimeInfo, e artifact.RuntimeEntry) {
	if info.Manifest != "" || e.Build == "" {
		return
	}
	builds, ok := s.serverBuilds()
	if !ok {
		return
	}
	if m, err := builds.BuildManifest(ctx, e.Build); err == nil {
		info.Manifest = string(m)
	}
}

// catalogInfo describes a catalog entry with the count of the connected
// workers that run it.
func catalogInfo(e artifact.RuntimeEntry, h worker.Hello, c artifact.WorkerCount) *RuntimeInfo {
	return &RuntimeInfo{
		Name: e.Name, CollectorVersion: h.CollectorVersion, ProductVersion: h.ProductVersion, Protocol: h.Protocol,
		Verified: h.Verified, Providers: slices.Clone(h.Providers), Manifest: string(e.Manifest),
		Catalog: &CatalogRuntime{Identity: e.Identity, Image: e.Image, Digest: e.Digest, Binary: runtimeBinary(e.Binary), Build: e.Build, Suspect: suspicion(e.Suspect), RegisteredBy: e.RegisteredBy,
			Registered: e.Registered, Confirmed: e.Confirmed, Workers: c.Workers, Slots: c.Slots},
	}
}

// workerCounts counts the connected workers by runtime identity: those of
// every instance when instances share a repository (presence.go), else this
// service's.
func (s *Service) workerCounts(ctx context.Context) (map[string]artifact.WorkerCount, error) {
	if s.shared {
		counts, err := s.repository.WorkerCounts(ctx, s.now())
		if err != nil {
			return nil, classify(err, CodeStorageError)
		}
		return counts, nil
	}
	counts := map[string]artifact.WorkerCount{}
	for _, w := range s.presentWorkers() {
		counts[w.Identity] = counts[w.Identity].Add(w.Capacity, false)
		if w.Own != "" {
			counts[w.Own] = counts[w.Own].Add(w.Capacity, true)
		}
	}
	return counts, nil
}

// confirmRuntimes confirms the catalog entries of a worker that said hello.
func (s *Service) confirmRuntimes(identity string) {
	if err := s.catalog.ConfirmRuntimes(s.ctx, identity, s.now()); err != nil && s.logger != nil {
		s.logError("confirm runtime catalog entries", err)
	}
}

// deleteCatalogRuntime removes name's catalog entry; ok is false when the
// catalog has no such name. A runtime with queued or running runs is a
// conflict.
func (s *Service) deleteCatalogRuntime(ctx context.Context, name string) (ok bool, err error) {
	e, _, ok, err := s.catalogEntry(ctx, name)
	if !ok || err != nil {
		return false, err
	}
	if s.runtimeInUse(pooledKey(e.Identity)) {
		return true, &Error{Code: CodeConflict, Message: fmt.Sprintf("runtime %s has queued or running runs; cancel them or wait", name)}
	}
	err = s.catalog.DeleteRuntime(ctx, name)
	if errors.Is(err, artifact.ErrConflict) {
		return true, &Error{Code: CodeConflict, Message: fmt.Sprintf(
			"runtime %s is the default runtime; make another runtime the default first (linea runtime default NAME)", name), cause: err}
	}
	return true, classify(err, CodeStorageError)
}

// pooledRuntime binds a catalog runtime to the workers of its identity,
// whether or not one is connected yet, and when it was built on the server
// (stored is its binary) to the pool workers of its binary's platform.
func pooledRuntime(s *Service, name string, h worker.Hello, stored *artifact.RuntimeBinary) *boundRuntime {
	target := engine.RuntimeTarget{ID: h.Runtime, Name: name}
	var binary *artifact.RunBinary
	if stored != nil {
		binary = &artifact.RunBinary{Digest: stored.Digest, Bytes: stored.Bytes, Platform: stored.OS + "/" + stored.Arch}
	}
	return &boundRuntime{
		info: RuntimeInfo{Name: name, CollectorVersion: h.CollectorVersion, ProductVersion: h.ProductVersion,
			Protocol: h.Protocol, Verified: h.Verified},
		worker: &poolWorker{s: s, target: target, binary: binary, gates: h.Gates},
		target: target,
		pooled: true,
		binary: binary,
	}
}

// pooledKey is a catalog runtime's key among bound runtimes.
func pooledKey(identity string) string { return "pooled:" + identity }

// CatalogRuntime is what the catalog records about a registered runtime
// and how many of its workers are connected, to any instance.
type CatalogRuntime struct {
	Identity string
	Image    string
	Digest   string
	// Binary is the stored worker binary of a runtime built on the server, and
	// Build the build that produced it; Image is then empty.
	Binary *RuntimeBinary
	Build  string
	// Suspect is set once a pool worker found Binary wrong (suspect.go);
	// runs on the runtime fail until it is registered or built again.
	Suspect      *Suspicion
	RegisteredBy string
	Registered   time.Time
	// Confirmed is zero until a connected worker of Identity said hello.
	Confirmed time.Time
	// Workers counts the connected workers that run Identity, the pool
	// workers of Binary's platform included, and Slots their capacity.
	Workers int
	Slots   int
}

// RuntimeBinary is a stored worker binary: its digest ("sha256:<hex>"),
// size and the platform it runs on.
type RuntimeBinary struct {
	Digest string
	Bytes  int64
	OS     string
	Arch   string
}

// Suspicion is why a pool worker distrusts a runtime binary: the failure it
// reported, the pool (pool:<os>/<arch>) and when.
type Suspicion struct {
	Cause string
	Pool  string
	At    time.Time
}

func suspicion(m *artifact.Suspicion) *Suspicion {
	if m == nil {
		return nil
	}
	return &Suspicion{Cause: m.Cause, Pool: m.Pool, At: m.At}
}

func runtimeBinary(b *artifact.RuntimeBinary) *RuntimeBinary {
	if b == nil {
		return nil
	}
	return &RuntimeBinary{Digest: b.Digest, Bytes: b.Bytes, OS: b.OS, Arch: b.Arch}
}
