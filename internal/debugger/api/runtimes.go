package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// Built runtimes: debug Collectors built from OCB manifests that
// runs, inspections, previews, suites and comparisons select with their
// runtime field. Building, listing, deleting, pushing and importing are
// platform operations; this adapter never calls Docker.

// Runtime is a built runtime, from its image's labels, or a runtime of the
// catalog (catalog set), from its registered hello: a catalog runtime has
// no image ID, tags, manifest or size.
type Runtime struct {
	Name             string   `json:"name"`
	ImageID          string   `json:"image_id"`
	Tags             []string `json:"tags"`
	CollectorVersion string   `json:"collector_version"`
	ProductVersion   string   `json:"product_version"`
	// Protocol is the worker protocol version of the image's worker.
	Protocol int `json:"protocol"`
	// Verified is false for a Collector release newer than the support
	// table.
	Verified bool `json:"verified"`
	// Default is true for the runtime that requests naming none run on.
	Default bool `json:"default,omitempty"`
	// Manifest is the OCB manifest the runtime was built from.
	Manifest  string `json:"manifest"`
	ModuleSet string `json:"module_set"`
	GoImage   string `json:"go_image"`
	// CustomModules are the modules built from supplied sources, with the
	// commit of a git source.
	CustomModules []platform.CustomModule `json:"custom_modules"`
	// Providers are the debugger providers the custom modules export.
	Providers []platform.RuntimeProvider `json:"providers,omitempty"`
	// Size is the image's size in bytes, 0 when unknown.
	Size int64 `json:"size"`
	// Catalog is set for a runtime of the catalog, which
	// connected workers serve.
	Catalog *RuntimeCatalogEntry `json:"catalog,omitempty"`
	// Projects are the projects the runtime is available to, IDs or "*"
	// alone; empty when it is available to every project.
	Projects []string `json:"projects,omitempty"`
}

// RuntimeCatalogEntry is what the catalog records about a registered
// runtime, with the workers of its identity connected to this instance.
type RuntimeCatalogEntry struct {
	// Identity is the runtime identity of the registered hello
	// ("sha256:<hex>"); requests run on workers whose hello reports it.
	Identity string `json:"identity"`
	// Image is the image reference the hello was read from, and Digest its
	// registry digest when known; both are empty for a runtime built on the
	// server.
	Image  string `json:"image"`
	Digest string `json:"digest,omitempty"`
	// Binary is the stored worker binary of a runtime built on the server,
	// and Build the ID of the build that produced it.
	Binary *RuntimeBinary `json:"binary,omitempty"`
	Build  string         `json:"build,omitempty"`
	// Suspect is set once a pool worker found the binary wrong; runs on
	// the runtime fail until it is registered or built again.
	Suspect      *RuntimeSuspicion `json:"suspect,omitempty"`
	RegisteredBy string            `json:"registered_by"`
	RegisteredAt time.Time         `json:"registered_at"`
	// ConfirmedAt is when a connected worker of the identity first said
	// hello; absent until then.
	ConfirmedAt *time.Time `json:"confirmed_at,omitempty"`
	// Workers counts the connected workers of the identity and Slots
	// their capacity.
	Workers int `json:"workers"`
	Slots   int `json:"slots"`
}

// RuntimeRegisterRequest registers a catalog runtime: a name, the image the
// hello was read from and that image's worker hello, as linea runtime
// register reads it where Docker runs.
type RuntimeRegisterRequest struct {
	Name   string `json:"name"`
	Image  string `json:"image"`
	Digest string `json:"digest,omitempty"`
	// Hello is the worker's hello as the worker protocol encodes it.
	Hello json.RawMessage `json:"hello"`
}

// RuntimeDefault is the runtime of every request that names none: the
// built-in runtime or a runtime of the catalog. SetBy and Set say who chose
// it and when, and are absent until the team chose one.
type RuntimeDefault struct {
	Name  string     `json:"name"`
	SetBy string     `json:"set_by,omitempty"`
	Set   *time.Time `json:"set,omitempty"`
}

// RuntimeDefaultRequest chooses the default runtime: "built-in" or a runtime
// of the catalog.
type RuntimeDefaultRequest struct {
	Name string `json:"name"`
}

func runtimeDefaultOf(v *platform.DefaultRuntimeInfo) *RuntimeDefault {
	if v == nil {
		return nil
	}
	return &RuntimeDefault{Name: v.Name, SetBy: v.SetBy, Set: v.Set}
}

func runtimeOf(v *platform.RuntimeInfo) *Runtime {
	if v == nil {
		return nil
	}
	tags := v.Tags
	if tags == nil {
		tags = []string{}
	}
	custom := v.CustomModules
	if custom == nil {
		custom = []platform.CustomModule{}
	}
	out := &Runtime{Name: v.Name, ImageID: v.ImageID, Tags: tags, CollectorVersion: v.CollectorVersion, ProductVersion: v.ProductVersion,
		Protocol: v.Protocol, Verified: v.Verified, Default: v.Default, Manifest: v.Manifest, ModuleSet: v.ModuleSet, GoImage: v.GoImage, CustomModules: custom, Size: v.Size, Providers: v.Providers, Projects: v.Projects}
	if c := v.Catalog; c != nil {
		out.Catalog = &RuntimeCatalogEntry{Identity: c.Identity, Image: c.Image, Digest: c.Digest, Build: c.Build, RegisteredBy: c.RegisteredBy,
			RegisteredAt: c.Registered, ConfirmedAt: timeOrNil(c.Confirmed), Workers: c.Workers, Slots: c.Slots}
		if b := c.Binary; b != nil {
			out.Catalog.Binary = &RuntimeBinary{Digest: b.Digest, Bytes: b.Bytes, OS: b.OS, Arch: b.Arch}
		}
		if m := c.Suspect; m != nil {
			out.Catalog.Suspect = &RuntimeSuspicion{Cause: m.Cause, Pool: m.Pool, At: m.At}
		}
	}
	return out
}

// RuntimeSuspicion is why a pool worker distrusts a runtime's binary.
type RuntimeSuspicion struct {
	// Cause is the failure the pool reported, without URLs.
	Cause string `json:"cause"`
	// Pool names the pool by platform, as pool:<os>/<arch>.
	Pool string    `json:"pool"`
	At   time.Time `json:"at"`
}

// RuntimeBinary is a stored worker binary.
type RuntimeBinary struct {
	// Digest is the binary's SHA-256 ("sha256:<hex>") and Bytes its size.
	Digest string `json:"digest"`
	Bytes  int64  `json:"bytes"`
	// OS and Arch are the Go platform it runs on, such as linux and amd64.
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

// RuntimeSource supplies one module's source: a directory, which only the
// CLI's own builds read (a team server reads no host directory; a build
// uploads it packed), or a git repository cloned with the server's git at
// ref (a branch, tag or commit; empty means HEAD), the module in subdir.
type RuntimeSource struct {
	Module string `json:"module"`
	Dir    string `json:"dir,omitempty"`
	Git    string `json:"git,omitempty"`
	Ref    string `json:"ref,omitempty"`
	Subdir string `json:"subdir,omitempty"`
}

// SourceOverrides maps sources to the platform's.
func SourceOverrides(list []RuntimeSource) []platform.SourceOverride {
	out := make([]platform.SourceOverride, 0, len(list))
	for _, s := range list {
		out = append(out, platform.SourceOverride{Module: s.Module, Dir: s.Dir, Git: s.Git, Ref: s.Ref, Subdir: s.Subdir})
	}
	return out
}

func timeOrNil(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func orEmpty(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

// CollectorSupport is the support table: runtimes build for every release
// from oldest on; verified releases have a dependency set and a catalog,
// newer ones build unverified.
type CollectorSupport struct {
	Oldest   string   `json:"oldest"`
	Verified []string `json:"verified"`
	// BuiltIn is the built-in runtime's release.
	BuiltIn string `json:"built_in"`
}

// ManifestRequest writes an OCB manifest from a verified release's catalog:
// a preset ("all" or "built-in"), or the components a
// configuration defines together with picked "<kind>/<type>" components,
// plus an entry for each component package of the custom sources.
type ManifestRequest struct {
	// Version is the release; empty means the newest verified one.
	Version    string                `json:"version,omitempty"`
	Preset     string                `json:"preset,omitempty"`
	ConfigYAML string                `json:"config_yaml,omitempty"`
	ConfigRef  *platform.ArtifactRef `json:"config_ref,omitempty"`
	Components []string              `json:"components,omitempty"`
	// Sources are custom module sources, as a build takes them.
	Sources []RuntimeSource `json:"sources,omitempty"`
}

// ComposedManifest is a written manifest; unknown names requested
// components the catalog does not know, warnings selected components the
// debugger does not support, custom the "<kind>/<import path>" entries
// added for the sources' components, providers the debugger providers
// found in the sources as "<name> <import path>".
type ComposedManifest struct {
	Manifest  string   `json:"manifest"`
	Unknown   []string `json:"unknown"`
	Warnings  []string `json:"warnings"`
	Custom    []string `json:"custom"`
	Providers []string `json:"providers"`
}

// RuntimePushRequest names the image reference to push a runtime as.
type RuntimePushRequest struct {
	Target string `json:"target"`
}

// RuntimeImportRequest accepts an image as a runtime: one in the server's
// Docker store, or pulled first.
type RuntimeImportRequest struct {
	Ref  string `json:"ref"`
	Pull bool   `json:"pull,omitempty"`
}

func (s *Server) listRuntimes(w http.ResponseWriter, r *http.Request) {
	list, err := s.runs.ListRuntimes(r.Context(), callerOf(r))
	if err != nil {
		writeOperationError(w, err)
		return
	}
	out := make([]Runtime, 0, len(list))
	for i := range list {
		out = append(out, *runtimeOf(&list[i]))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getRuntime(w http.ResponseWriter, r *http.Request) {
	info, err := s.runs.GetRuntime(r.Context(), callerOf(r), r.PathValue("name"))
	if err != nil {
		writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, runtimeOf(info))
}

func (s *Server) deleteRuntime(w http.ResponseWriter, r *http.Request) {
	if err := s.runs.DeleteRuntime(r.Context(), callerOf(r), r.PathValue("name")); err != nil {
		writeOperationError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) pushRuntime(w http.ResponseWriter, r *http.Request) {
	var req RuntimePushRequest
	if !decode(w, r, &req) {
		return
	}
	if err := s.runs.PushRuntime(r.Context(), callerOf(r), platform.PushRuntimeRequest{Runtime: r.PathValue("name"), Target: req.Target}); err != nil {
		writeOperationError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) importRuntime(w http.ResponseWriter, r *http.Request) {
	var req RuntimeImportRequest
	if !decode(w, r, &req) {
		return
	}
	info, err := s.runs.ImportRuntime(r.Context(), callerOf(r), platform.ImportRuntimeRequest{Ref: req.Ref, Pull: req.Pull})
	if err != nil {
		writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, runtimeOf(info))
}

func (s *Server) registerRuntime(w http.ResponseWriter, r *http.Request) {
	var req RuntimeRegisterRequest
	if !decode(w, r, &req) {
		return
	}
	info, err := s.runs.RegisterRuntime(r.Context(), callerOf(r), platform.RegisterRuntimeRequest{Name: req.Name, Image: req.Image, Digest: req.Digest, Hello: req.Hello})
	if err != nil {
		writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, runtimeOf(info))
}

func (s *Server) getDefaultRuntime(w http.ResponseWriter, r *http.Request) {
	info, err := s.runs.GetDefaultRuntime(r.Context(), callerOf(r))
	if err != nil {
		writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, runtimeDefaultOf(info))
}

func (s *Server) setDefaultRuntime(w http.ResponseWriter, r *http.Request) {
	var req RuntimeDefaultRequest
	if !decode(w, r, &req) {
		return
	}
	info, err := s.runs.SetDefaultRuntime(r.Context(), callerOf(r), req.Name)
	if err != nil {
		writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, runtimeDefaultOf(info))
}

func (s *Server) getCollectorSupport(w http.ResponseWriter, r *http.Request) {
	v, err := s.runs.GetCollectorSupport(r.Context(), callerOf(r))
	if err != nil {
		writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, CollectorSupport{Oldest: v.Oldest, Verified: orEmpty(v.Verified), BuiltIn: v.BuiltIn})
}

func (s *Server) getCollectorCatalog(w http.ResponseWriter, r *http.Request) {
	cat, err := s.runs.GetCollectorCatalog(r.Context(), callerOf(r), r.PathValue("version"))
	if err != nil {
		writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cat)
}

func (s *Server) composeManifest(w http.ResponseWriter, r *http.Request) {
	var req ManifestRequest
	if !decode(w, r, &req) {
		return
	}
	in := platform.ComposeManifestRequest{Version: req.Version, Preset: req.Preset, Components: req.Components, Sources: SourceOverrides(req.Sources)}
	if req.ConfigYAML != "" || req.ConfigRef != nil {
		in.Configuration = &platform.Source{Text: req.ConfigYAML, Artifact: req.ConfigRef}
	}
	m, err := s.runs.ComposeManifest(r.Context(), callerOf(r), in)
	if err != nil {
		writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ComposedManifest{Manifest: m.Manifest, Unknown: orEmpty(m.Unknown), Warnings: orEmpty(m.Warnings), Custom: orEmpty(m.Custom), Providers: orEmpty(m.Providers)})
}

func (s *Server) routeRuntimes() {
	s.mux.HandleFunc("GET /api/v1/runtimes", s.listRuntimes)
	s.mux.HandleFunc("POST /api/v1/runtimes/import", s.importRuntime)
	s.mux.HandleFunc("POST /api/v1/runtimes/register", s.registerRuntime)
	s.mux.HandleFunc("GET /api/v1/runtime-default", s.getDefaultRuntime)
	s.mux.HandleFunc("PUT /api/v1/runtime-default", s.setDefaultRuntime)
	s.mux.HandleFunc("GET /api/v1/runtimes/{name}", s.getRuntime)
	s.mux.HandleFunc("DELETE /api/v1/runtimes/{name}", s.deleteRuntime)
	s.mux.HandleFunc("POST /api/v1/runtimes/{name}/push", s.pushRuntime)
	s.mux.HandleFunc("GET /api/v1/collector-versions", s.getCollectorSupport)
	s.mux.HandleFunc("GET /api/v1/collector-versions/{version}/catalog", s.getCollectorCatalog)
	s.mux.HandleFunc("POST /api/v1/manifests", s.composeManifest)
	s.mux.HandleFunc("GET /api/v1/runtime-storage", s.getRuntimeStorage)
	s.mux.HandleFunc("POST /api/v1/runtime-storage/prune", s.pruneRuntimes)
}

const noDocker = "The server has no runtime store (Docker), or Docker does not answer."

func runtimePaths() map[string]any {
	name := []any{map[string]any{"name": "name", "in": "path", "required": true, "schema": map[string]any{"type": "string"},
		"description": "A runtime name, image reference or image ID."}}
	array := func(schema string) map[string]any { return map[string]any{"type": "array", "items": ref(schema)} }
	return map[string]any{
		"/api/v1/runtimes": map[string]any{"get": op("listRuntimes",
			"The runtimes of the catalog and the built runtimes in the server's Docker image store, by name; a catalog name wins over a built runtime of the same name. A team server has no Docker store and lists only the catalog. The built-in runtime is not listed; requests select it by omitting runtime or naming \"built-in\".",
			nil, "", map[string]any{
				"200": reply("The runtimes.", array("Runtime")),
				"503": operationFailure(noDocker),
			})},
		"/api/v1/runtimes/import": map[string]any{"post": op("importRuntime",
			"Accept an image as a runtime, pulled first with pull, only if its worker speaks this debugger's protocol; it is then tagged linea-runtime:<name>.",
			nil, "RuntimeImportRequest", map[string]any{
				"200": reply("The imported runtime.", ref("Runtime")),
				"400": operationFailure("Malformed request, or an image that is not a debugger runtime."),
				"404": operationFailure("The image is not in the server's Docker store."),
				"409": operationFailure("The image's worker speaks another protocol (unsupported_build): build it again with this debugger."),
				"503": operationFailure(noDocker),
			})},
		"/api/v1/runtimes/register": map[string]any{"post": op("registerRuntime",
			"Register a catalog runtime: the name requests select, the image the hello was read from and that image's worker hello, which linea runtime register reads where Docker runs. It replaces an entry of the same name. Runs on it queue until a connected worker whose hello reports the identity takes them; that worker's hello confirms the entry. The server runs no container.",
			nil, "RuntimeRegisterRequest", map[string]any{
				"200": reply("The registered runtime.", ref("Runtime")),
				"400": operationFailure("Malformed request, invalid or built-in name, no image, an invalid digest or a hello without a runtime identity."),
				"409": operationFailure("The hello's worker speaks another protocol (unsupported_build): build it again with this debugger."),
				"413": operationFailure("The catalog is full."),
			})},
		"/api/v1/runtime-default": map[string]any{
			"get": op("getDefaultRuntime", "The default runtime: the runtime of every request that names none, with who chose it and when. Until the team chose one it is the built-in runtime, or the runtime the server was started with.", nil, "", map[string]any{
				"200": reply("The default runtime.", ref("RuntimeDefault")),
			}),
			"put": op("setDefaultRuntime", "Choose the default runtime of every server instance: the built-in runtime or a runtime of the catalog. Requests that name no runtime then run on it, and it cannot be removed until another is chosen.", nil, "RuntimeDefaultRequest", map[string]any{
				"200": reply("The default runtime.", ref("RuntimeDefault")),
				"400": operationFailure("Malformed request or an invalid runtime name."),
				"404": operationFailure("The runtime is not in the catalog."),
			}),
		},
		"/api/v1/runtimes/{name}": map[string]any{
			"get": op("getRuntime", "One runtime.", name, "", map[string]any{
				"200": reply("The runtime.", ref("Runtime")),
				"400": operationFailure("The built-in runtime was named."),
				"404": operationFailure("Unknown runtime."),
				"503": operationFailure(noDocker),
			}),
			"delete": op("deleteRuntime", "Remove a catalog entry, or else a built runtime's image, whose idle worker stops first. Connected workers stay connected.", name, "", map[string]any{
				"204": map[string]any{"description": "The runtime was removed."},
				"400": operationFailure("The built-in runtime was named."),
				"404": operationFailure("Unknown runtime."),
				"409": operationFailure("A queued or running run uses the runtime, or it is the default runtime."),
				"503": operationFailure(noDocker),
			}),
		},
		"/api/v1/runtimes/{name}/push": map[string]any{"post": op("pushRuntime",
			"Tag a runtime's image as target and push it with the server user's Docker credentials.",
			name, "RuntimePushRequest", map[string]any{
				"204": map[string]any{"description": "The image was pushed."},
				"400": operationFailure("Malformed request or no target."),
				"404": operationFailure("Unknown runtime."),
				"500": operationFailure("Docker could not push the image."),
				"503": operationFailure(noDocker),
			})},
		"/api/v1/collector-versions": map[string]any{"get": op("getCollectorSupport",
			"The support table: the oldest release runtimes build for and the verified releases, which have a component catalog.",
			nil, "", map[string]any{
				"200": reply("The support table.", ref("CollectorSupport")),
			})},
		"/api/v1/collector-versions/{version}/catalog": map[string]any{"get": op("getCollectorCatalog",
			"A verified release's component catalog: every core and contrib component with its module, type, kind and stability.",
			[]any{map[string]any{"name": "version", "in": "path", "required": true, "schema": map[string]any{"type": "string"}, "description": "A verified Collector release, such as 0.162.0."}},
			"", map[string]any{
				"200": reply("The catalog.", ref("CollectorCatalog")),
				"404": operationFailure("The release is not verified by this debugger."),
			})},
		"/api/v1/manifests": map[string]any{"post": op("composeManifest",
			"Write an OCB manifest from a verified release's catalog: a preset, or the components a configuration defines with picked ones. It never saves or builds.",
			nil, "ManifestRequest", map[string]any{
				"200": reply("The manifest.", ref("ComposedManifest")),
				"400": operationFailure("Malformed request, unknown preset or component syntax."),
				"403": operationFailure(forbiddenPath),
				"404": operationFailure("The release is not verified by this debugger."),
				"422": operationFailure("The configuration could not be read."),
			})},
	}
}
