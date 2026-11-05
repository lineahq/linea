package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/lineahq/linea/build/versions"
	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/authz"
	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/container"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/release"
	"github.com/lineahq/linea/internal/debugger/worker"
)

// Built runtimes: a debug Collector built from a manifest runs in
// a worker container, and every run, inspection, preview, suite and
// comparison may name one. Docker's image store
// lists them; the runtime catalog (runtime_catalog.go) lists the runtimes
// of connected workers.

// BuiltInRuntime names the debugger's own registry where a request chooses
// a runtime; an empty name means the same.
const BuiltInRuntime = "built-in"

// RuntimeInfo describes a built runtime from its image's labels, or a
// catalog runtime from its registered hello (Catalog set, no image fields).
type RuntimeInfo struct {
	Name             string
	ImageID          string
	Tags             []string
	CollectorVersion string
	ProductVersion   string
	// Protocol is the worker protocol version the image's worker speaks.
	Protocol int
	// Verified is false for a Collector version newer than the support
	// table.
	Verified bool
	// Manifest is the OCB manifest the runtime was built from, unchanged.
	Manifest  string
	ModuleSet string
	GoImage   string
	// CustomModules are the modules built from supplied sources.
	CustomModules []CustomModule
	// Providers are the debugger providers the custom modules export.
	Providers []RuntimeProvider
	// Size is the image's size in bytes, when the store knows it.
	Size int64
	// Catalog is set for a runtime of the catalog.
	Catalog *CatalogRuntime
	// Default is set for the default runtime.
	Default bool
	// Projects are the projects the runtime is available to, IDs or "*"
	// alone; empty when it is available to every project.
	Projects []string
	// Reference is set for a runtime image read from a registry:
	// the image by digest. Binary is its cached binary, which runs as a
	// child process instead of a container.
	Reference string
	Binary    string
}

// CustomModule is a module a runtime was built from a supplied source,
// with the git commit when the source was a repository.
type CustomModule = engine.ReportCustomModule

// RuntimeProvider is a component module's debugger provider in a runtime.
type RuntimeProvider = engine.ReportProvider

// manifestDigest is "sha256:<hex>" over the manifest bytes.
func (i *RuntimeInfo) manifestDigest() string {
	sum := sha256.Sum256([]byte(i.Manifest))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// RuntimeBuildSpec is a build a RuntimeStore performs.
type RuntimeBuildSpec struct {
	Name     string
	Manifest []byte
	// Release is the manifest's release in the support table, with its
	// dependency set; nil builds an unverified runtime.
	Release *versions.Release
	// Sources are the supplied module sources; git sources have their
	// Files and Commit once the build starts.
	Sources []BuildSource
	// GoImage overrides the pinned official Go image.
	GoImage string
	// ModuleProxy is GOPROXY for this build; empty passes the debugger's
	// module environment through.
	ModuleProxy string
	// Offline builds without network access, from the cache volumes only.
	Offline bool
}

// RuntimeStore builds, lists and starts runtimes: DockerRuntimes in
// production, a double in tests. Errors wrap ErrRuntimeNotFound for an
// unknown runtime and engine.ErrRuntimeUnavailable when the store cannot be
// used (no Docker); Check returns *config.DiagnosticsError for a build it
// refuses before compiling.
type RuntimeStore interface {
	Check(spec RuntimeBuildSpec) error
	Build(ctx context.Context, spec RuntimeBuildSpec, log func(string)) (*RuntimeInfo, error)
	List(ctx context.Context) ([]RuntimeInfo, error)
	// Get finds a runtime by name, image reference or image ID.
	Get(ctx context.Context, ref string) (*RuntimeInfo, error)
	Delete(ctx context.Context, info *RuntimeInfo) error
	Push(ctx context.Context, info *RuntimeInfo, target string) error
	// Import accepts an image reference already in the store, or pulls it,
	// and makes it nameable only if accept returns nil.
	Import(ctx context.Context, ref string, pull bool, accept func(*RuntimeInfo) error) (*RuntimeInfo, error)
	// Start runs the runtime's worker; the service keeps one per runtime.
	Start(ctx context.Context, info *RuntimeInfo) (RuntimeWorker, error)
}

// RuntimeWorker is a started runtime's worker.
type RuntimeWorker interface {
	Runtime
	Close() error
}

// ErrRuntimeNotFound is a runtime the store does not have.
var ErrRuntimeNotFound = errors.New("runtime not found")

// PushRuntimeRequest pushes a runtime image to a registry the user names,
// with the user's Docker credentials.
type PushRuntimeRequest struct {
	Runtime string
	// Target is the image reference to push, for example
	// registry.example.com/team/linea-runtime:0.162.
	Target string
}

// ImportRuntimeRequest accepts a runtime image for use, typically in CI.
type ImportRuntimeRequest struct {
	// Ref is an image reference or ID.
	Ref string
	// Pull pulls Ref first; otherwise it must already be in Docker's store.
	Pull bool
}

// boundRuntime is a runtime with its started worker.
type boundRuntime struct {
	info   RuntimeInfo
	worker RuntimeWorker
	target engine.RuntimeTarget
	// pooled is set for the runtime of connected workers (worker_pool.go),
	// which the service never starts or stops.
	pooled bool
	// binary is a pooled runtime's stored binary when it was built on the
	// server; its runs are pinned to it.
	binary *artifact.RunBinary
}

// key is the runtime's entry in the service's workers: its image, or its
// identity when connected workers serve it.
func (b *boundRuntime) key() string {
	if b.pooled {
		return pooledKey(b.target.ID)
	}
	return b.info.ImageID
}

// stamp records the runtime in a report its worker produced. The
// worker's own description supplies the feature gates of its registry and
// the optional features it lacks. A connected worker's runtime is
// recorded as remote, by name.
func (b *boundRuntime) stamp(rep *engine.Report) {
	rt := b.describe()
	if rep.Runtime != nil {
		rt.FeatureGates = rep.Runtime.FeatureGates
		rt.MissingFeatures = rep.Runtime.MissingFeatures
		rt.Providers = rep.Runtime.Providers
	}
	if rt.FeatureGates == nil {
		rt.FeatureGates = []release.FeatureGate{}
	}
	rep.Runtime = rt
}

// describe is the runtime as a report records it, without what only its
// worker knows: feature gates, missing features and providers.
func (b *boundRuntime) describe() *engine.ReportRuntime {
	if b.pooled && b.info.Name == BuiltInRuntime {
		// A worker of this release: the debugger's own registry.
		return &engine.ReportRuntime{Kind: engine.RuntimeBuiltIn, Name: BuiltInRuntime, CollectorVersion: b.info.CollectorVersion, Verified: b.info.Verified}
	}
	if b.pooled {
		return &engine.ReportRuntime{Kind: engine.RuntimeRemote, Name: b.info.Name,
			CollectorVersion: b.info.CollectorVersion, Verified: b.info.Verified}
	}
	kind, name := engine.RuntimeContainer, b.info.Name
	if b.info.Reference != "" {
		kind, name = engine.RuntimeImage, b.info.Reference
	}
	return &engine.ReportRuntime{
		Kind: kind, Name: name, ImageID: b.info.ImageID,
		CollectorVersion: b.info.CollectorVersion, ManifestDigest: b.info.manifestDigest(),
		ModuleSet: b.info.ModuleSet, Verified: b.info.Verified, CustomModules: slices.Clone(b.info.CustomModules),
	}
}

// answeredBy is the runtime an inspection or preview ran on:
// this process's registry when rt is nil.
func answeredBy(rt Runtime) *engine.ReportRuntime {
	if u, ok := rt.(*unbindOnFailure); ok {
		d := u.b.describe()
		d.FeatureGates = []release.FeatureGate{}
		return d
	}
	if rt == nil {
		return engine.BuiltInRuntime()
	}
	return nil
}

// selectsRuntime reports whether a request's runtime field names a built
// runtime rather than the built-in one.
func selectsRuntime(name string) bool { return name != "" && name != BuiltInRuntime }

// runtimeName is the runtime a request's runtime field selects: the named
// one, else the service's default runtime, else BuiltInRuntime.
func (s *Service) runtimeName(name string) string {
	if s.selects(name) {
		return name
	}
	if d := s.defaultRuntimeName(); d != "" {
		return d
	}
	return BuiltInRuntime
}

// runtimeSelection marks an engine request for a runtime until preparation
// binds it: a target with a name and no ID. The built-in one is marked too,
// for a team server to bind (Service.selects).
func runtimeSelection(name string) *engine.RuntimeTarget {
	if name == "" {
		return nil
	}
	return &engine.RuntimeTarget{Name: name}
}

func errNoRuntimeStore() error {
	return &Error{Code: CodeUnavailable, Message: "built runtimes need Docker, and this service has none; run on the built-in runtime"}
}

func (s *Service) runtimeStore() (RuntimeStore, error) {
	if s.runtimes == nil && s.workerToken != "" {
		return nil, &Error{Code: CodeUnavailable, Message: "this server builds and stores no runtimes: build one where Docker runs, push it and register it with linea runtime register"}
	}
	if s.runtimes == nil {
		return nil, errNoRuntimeStore()
	}
	return s.runtimes, nil
}

// checkProtocol refuses a runtime whose worker speaks another protocol.
func checkProtocol(info *RuntimeInfo) error {
	if info.Protocol != worker.ProtocolVersion {
		return &Error{Code: CodeUnsupportedBuild, Message: fmt.Sprintf(
			"runtime %s speaks worker protocol %d, this debugger speaks %d: build it again with this debugger", info.Name, info.Protocol, worker.ProtocolVersion)}
	}
	return nil
}

// bind returns the started worker of the named runtime, starting it on
// first use: the catalog's runtime first, served by connected workers, then the
// store's. A runtime neither has is unavailable, naming it.
func (s *Service) bind(ctx context.Context, name string) (*boundRuntime, error) {
	if name == BuiltInRuntime && s.builtInOnWorkers {
		return s.bindBuiltIn(ctx)
	}
	if container.IsImageReference(name) {
		return s.bindImage(ctx, name)
	}
	if e, h, ok, err := s.catalogEntry(ctx, name); err != nil || ok {
		if err != nil {
			return nil, err
		}
		if e.Suspect != nil && e.Binary != nil {
			return nil, suspectError(name, e.Suspect)
		}
		return pooledRuntime(s, name, h, e.Binary), nil
	}
	if s.runtimes == nil && s.workerToken != "" {
		return nil, &Error{Code: CodeUnavailable, Message: fmt.Sprintf("runtime %s is not available: register it with linea runtime register", name)}
	}
	store, err := s.runtimeStore()
	if err != nil {
		return nil, err
	}
	info, err := store.Get(ctx, name)
	if errors.Is(err, ErrRuntimeNotFound) {
		return nil, &Error{Code: CodeUnavailable, Message: fmt.Sprintf("runtime %s is not available: build or import it first", name), cause: err}
	}
	if err != nil {
		return nil, classify(err, CodeInternal)
	}
	return s.start(ctx, store, info)
}

// start returns the started worker of a store's runtime, starting it on
// first use.
func (s *Service) start(ctx context.Context, store RuntimeStore, info *RuntimeInfo) (*boundRuntime, error) {
	if err := checkProtocol(info); err != nil {
		return nil, err
	}
	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()
	if s.workers == nil {
		return nil, ErrClosed
	}
	if b := s.workers[info.ImageID]; b != nil {
		return b, nil
	}
	w, err := store.Start(ctx, info)
	if err != nil {
		return nil, classify(fmt.Errorf("runtime %s: %w", info.Name, err), CodeInternal)
	}
	target := w.Target()
	target.Name = info.Name
	b := &boundRuntime{info: *info, worker: w, target: target}
	s.workers[info.ImageID] = b
	return b, nil
}

// unbind closes a runtime's worker after it failed, so that the next use
// starts a new one.
func (s *Service) unbind(b *boundRuntime) {
	s.runtimeMu.Lock()
	if s.workers[b.key()] == b {
		delete(s.workers, b.key())
	}
	s.runtimeMu.Unlock()
	_ = b.worker.Close()
}

// closeRuntimes closes every started worker; the service is closing.
func (s *Service) closeRuntimes() {
	s.runtimeMu.Lock()
	workers := s.workers
	s.workers = nil
	s.runtimeMu.Unlock()
	for _, b := range workers {
		_ = b.worker.Close()
	}
}

// ListRuntimes lists the catalog's runtimes and the built runtimes in
// Docker's image store, by name.
func (s *Service) ListRuntimes(ctx context.Context, caller Caller) (_ []RuntimeInfo, err error) {
	defer s.audit(&caller, "ListRuntimes", &err)()
	if err := s.begin(ctx, caller, authz.RuntimeRead); err != nil {
		return nil, err
	}
	catalog, err := s.catalogRuntimes(ctx)
	if err != nil {
		return nil, err
	}
	var list []RuntimeInfo
	// A server that accepts workers and has no Docker lists no images.
	if s.runtimes != nil || s.workerToken == "" {
		store, err := s.runtimeStore()
		if err != nil {
			return nil, err
		}
		if list, err = store.List(ctx); err != nil {
			return nil, classify(err, CodeInternal)
		}
		// A replaced image is storage, not a runtime: nothing names it
		// (GetRuntimeStorage lists it).
		states, err := imageStates(ctx, store, list)
		if err != nil {
			return nil, classify(err, CodeInternal)
		}
		list = slices.DeleteFunc(list, func(info RuntimeInfo) bool { return states[info.ImageID] == RuntimeReplaced })
	}
	list = append(catalog, list...)
	def := s.defaultRuntimeName()
	if caller.Project != "" {
		def = s.projectDefaultRuntime(caller.Project)
		list = slices.DeleteFunc(list, func(info RuntimeInfo) bool { return !s.availableTo(info.Name, caller.Project) })
	}
	set := s.projectState()
	for i := range list {
		list[i].Default = list[i].Name == def && list[i].Catalog != nil
		if a, ok := set.Availability(list[i].Name); ok {
			list[i].Projects = slices.Clone(a.Projects)
		}
	}
	slices.SortStableFunc(list, func(a, b RuntimeInfo) int {
		return cmpStrings(a.Name+"\x00"+a.ImageID, b.Name+"\x00"+b.ImageID)
	})
	return list, nil
}

func cmpStrings(a, b string) int { return strings.Compare(a, b) }

// GetRuntime returns one runtime: the catalog's by name, else a built one
// by name, image reference or image ID.
func (s *Service) GetRuntime(ctx context.Context, caller Caller, name string) (_ *RuntimeInfo, err error) {
	defer s.audit(&caller, "GetRuntime", &err)()
	if err := s.begin(ctx, caller, authz.RuntimeRead); err != nil {
		return nil, err
	}
	if name == BuiltInRuntime && s.builtInOnWorkers {
		info, err := s.builtInInfo(ctx)
		if info != nil {
			info.Default = s.defaultRuntimeName() == BuiltInRuntime
		}
		return info, err
	}
	if selectsRuntime(name) {
		if e, h, ok, err := s.catalogEntry(ctx, name); err != nil || ok {
			if err != nil {
				return nil, err
			}
			counts, err := s.workerCounts(ctx)
			if err != nil {
				return nil, err
			}
			info := catalogInfo(e, h, counts[e.Identity])
			s.buildManifest(ctx, info, e)
			info.Default = s.defaultRuntimeName() == name
			return info, nil
		}
	}
	return s.getRuntime(ctx, name)
}

// getRuntime returns a built runtime from the store.
func (s *Service) getRuntime(ctx context.Context, name string) (*RuntimeInfo, error) {
	if !selectsRuntime(name) {
		return nil, &Error{Code: CodeInvalidRequest, Message: "name a built runtime; the built-in runtime is part of the debugger"}
	}
	store, err := s.runtimeStore()
	if err != nil {
		return nil, err
	}
	info, err := store.Get(ctx, name)
	if errors.Is(err, ErrRuntimeNotFound) {
		return nil, &Error{Code: CodeNotFound, Message: fmt.Sprintf("runtime %s not found", name), cause: err}
	}
	return info, classify(err, CodeInternal)
}

// DeleteRuntime removes a catalog entry, or else a built runtime's image. A
// runtime with queued or running runs is a conflict; a built runtime's idle
// worker is stopped first. Connected workers stay connected.
func (s *Service) DeleteRuntime(ctx context.Context, caller Caller, name string) (err error) {
	defer s.audit(&caller, "DeleteRuntime", &err)()
	if err := s.begin(ctx, caller, authz.RuntimeManage); err != nil {
		return err
	}
	if selectsRuntime(name) {
		if ok, err := s.deleteCatalogRuntime(ctx, name); ok || err != nil {
			return err
		}
	}
	info, err := s.getRuntime(ctx, name)
	if err != nil {
		return err
	}
	if s.runtimeInUse(info.ImageID) {
		return &Error{Code: CodeConflict, Message: fmt.Sprintf("runtime %s has queued or running runs; cancel them or wait", info.Name)}
	}
	s.stopIdleWorker(info.ImageID)
	return classify(s.runtimes.Delete(ctx, info), CodeInternal)
}

// PushRuntime tags and pushes a runtime image.
func (s *Service) PushRuntime(ctx context.Context, caller Caller, req PushRuntimeRequest) (err error) {
	defer s.audit(&caller, "PushRuntime", &err)()
	if err := s.begin(ctx, caller, authz.RuntimeManage); err != nil {
		return err
	}
	if strings.TrimSpace(req.Target) == "" {
		return &Error{Code: CodeInvalidRequest, Message: "push needs a target image reference"}
	}
	info, err := s.getRuntime(ctx, req.Runtime)
	if err != nil {
		return err
	}
	return classify(s.runtimes.Push(ctx, info, req.Target), CodeInternal)
}

// ImportRuntime accepts a runtime image, pulled or already loaded, only if
// its worker speaks this debugger's protocol.
func (s *Service) ImportRuntime(ctx context.Context, caller Caller, req ImportRuntimeRequest) (_ *RuntimeInfo, err error) {
	defer s.audit(&caller, "ImportRuntime", &err)()
	if err := s.begin(ctx, caller, authz.RuntimeManage); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.Ref) == "" {
		return nil, &Error{Code: CodeInvalidRequest, Message: "import needs an image reference"}
	}
	store, err := s.runtimeStore()
	if err != nil {
		return nil, err
	}
	info, err := store.Import(ctx, req.Ref, req.Pull, checkProtocol)
	if errors.Is(err, ErrRuntimeNotFound) {
		return nil, &Error{Code: CodeNotFound, Message: fmt.Sprintf("image %s not found; pull it or name one in Docker's store", req.Ref), cause: err}
	}
	if err != nil {
		return nil, classify(err, CodeInvalidRequest)
	}
	return info, nil
}

// manifestDiagnostics turns problems found before building into the
// validation_failed diagnostics.
func manifestDiagnostics(code string, problems ...string) error {
	d := make([]config.Diagnostic, 0, len(problems))
	for _, p := range problems {
		d = append(d, config.Diagnostic{Severity: config.SeverityError, Code: code, Component: "manifest", Message: p})
	}
	return &config.DiagnosticsError{Diagnostics: d}
}

// runtimeFor is the runtime that completes an inspection or preview: the
// named runtime's worker, or nil for the built-in engine in process.
func (s *Service) runtimeFor(ctx context.Context, name string) (Runtime, error) {
	if name = s.runtimeName(name); !s.selects(name) {
		return nil, nil
	}
	b, err := s.bind(ctx, name)
	if err != nil {
		return nil, err
	}
	return &unbindOnFailure{s: s, b: b}, nil
}

// unbindOnFailure drops a worker that failed an inspection or preview, as
// a failed run does.
type unbindOnFailure struct {
	s *Service
	b *boundRuntime
}

func (u *unbindOnFailure) Target() engine.RuntimeTarget { return u.b.target }

// policyFor is the capture policy for a runtime: a connected worker receives
// only the environment values its policy lets leave the machine.
func (s *Service) policyFor(rt Runtime) engine.Policy {
	if u, ok := rt.(*unbindOnFailure); ok && u.b.pooled {
		return s.remotePolicy
	}
	return s.policy
}

func (u *unbindOnFailure) Execute(ctx context.Context, req engine.Request) (*engine.Report, error) {
	rep, err := u.b.worker.Execute(ctx, req)
	return rep, u.check(err)
}

func (u *unbindOnFailure) Inspect(ctx context.Context, in *engine.InspectionInput) (*engine.Inspection, error) {
	ins, err := u.b.worker.Inspect(ctx, in)
	return ins, u.check(err)
}

func (u *unbindOnFailure) EvaluateOTTL(ctx context.Context, in *engine.OTTLInput) (*engine.OTTLEvaluation, error) {
	ev, err := u.b.worker.EvaluateOTTL(ctx, in)
	return ev, u.check(err)
}

func (u *unbindOnFailure) check(err error) error {
	if errors.Is(err, engine.ErrRuntimeUnavailable) {
		u.s.unbind(u.b)
	}
	return err
}
