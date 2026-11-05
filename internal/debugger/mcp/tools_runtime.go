package mcp

import (
	"context"
	"fmt"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lineahq/linea/internal/debugger/platform"
)

type listRuntimesInput struct{}

type runtimeView struct {
	Name             string   `json:"name"`
	ImageID          string   `json:"image_id"`
	Tags             []string `json:"tags"`
	CollectorVersion string   `json:"collector_version"`
	ProductVersion   string   `json:"product_version,omitempty"`
	Protocol         int      `json:"protocol"`
	Verified         bool     `json:"verified"`
	// Default is true for the runtime requests that name none run on.
	Default   bool   `json:"default"`
	ModuleSet string `json:"module_set,omitempty"`
	// CustomModules are the modules built from supplied sources, with a git
	// source's commit.
	CustomModules []platform.CustomModule `json:"custom_modules"`
	// Providers are the debugger providers the custom modules export.
	Providers []platform.RuntimeProvider `json:"providers,omitempty"`
	// Catalog is set for a runtime of the catalog, which connected workers
	// serve.
	Catalog *catalogView `json:"catalog,omitempty"`
}

type catalogView struct {
	Identity     string     `json:"identity"`
	Image        string     `json:"image"`
	Digest       string     `json:"digest,omitempty"`
	BinaryDigest string     `json:"binary_digest,omitempty"`
	Platform     string     `json:"platform,omitempty"`
	Build        string     `json:"build,omitempty"`
	RegisteredAt time.Time  `json:"registered_at"`
	ConfirmedAt  *time.Time `json:"confirmed_at,omitempty"`
	Workers      int        `json:"workers"`
	Slots        int        `json:"slots"`
}

func runtimeOf(info *platform.RuntimeInfo) *runtimeView {
	if info == nil {
		return nil
	}
	tags := info.Tags
	if tags == nil {
		tags = []string{}
	}
	custom := info.CustomModules
	if custom == nil {
		custom = []platform.CustomModule{}
	}
	out := &runtimeView{Name: info.Name, ImageID: info.ImageID, Tags: tags, CollectorVersion: info.CollectorVersion,
		ProductVersion: info.ProductVersion, Protocol: info.Protocol, Verified: info.Verified, Default: info.Default, ModuleSet: info.ModuleSet, CustomModules: custom, Providers: info.Providers}
	if c := info.Catalog; c != nil {
		out.Catalog = &catalogView{Identity: c.Identity, Image: c.Image, Digest: c.Digest, Build: c.Build, RegisteredAt: c.Registered, Workers: c.Workers, Slots: c.Slots}
		if b := c.Binary; b != nil {
			out.Catalog.BinaryDigest, out.Catalog.Platform = b.Digest, b.OS+"/"+b.Arch
		}
		if !c.Confirmed.IsZero() {
			confirmed := c.Confirmed
			out.Catalog.ConfirmedAt = &confirmed
		}
	}
	return out
}

type runtimesResult struct {
	Runtimes []runtimeView `json:"runtimes"`
}

// summary counts runtimes and unverified ones; names and versions stay in
// the structured result.
func (r runtimesResult) summary() string {
	unverified := 0
	for _, rt := range r.Runtimes {
		if !rt.Verified {
			unverified++
		}
	}
	return fmt.Sprintf("Runtimes: %s, %d unverified; the built-in runtime is always available.", plural(len(r.Runtimes), "built runtime"), unverified)
}

type buildRuntimeInput struct {
	Name         string  `json:"name" jsonschema:"Runtime name: lower-case letters, digits, '.', '_' and '-', at most 63 characters. Building a name again replaces that runtime once the build succeeds."`
	Manifest     *source `json:"manifest,omitempty" jsonschema:"OCB manifest source (text or a saved manifest artifact; the server reads no manifest file); supply exactly one of manifest or manifest_yaml."`
	ManifestYAML *string `json:"manifest_yaml,omitempty" jsonschema:"OCB manifest text; supply exactly one of manifest_yaml or manifest. MCP uploads no module sources, so the manifest must not build a module from a local directory: use linea runtime build --server for that."`
	WaitSeconds  *int    `json:"wait_seconds,omitempty" jsonschema:"Seconds to wait for the build, from 0 to 120; defaults to 30. Builds take minutes; waiting never cancels the build."`
}

type getBuildInput struct {
	BuildID     string `json:"build_id" jsonschema:"Build ID returned by linea_build_runtime."`
	LogFrom     *int   `json:"log_from,omitempty" jsonschema:"First log line to return; defaults to the last 50 lines."`
	WaitSeconds *int   `json:"wait_seconds,omitempty" jsonschema:"Seconds to wait for the build to end, from 0 to 120; defaults to 0. Waiting never cancels the build."`
}

type buildResult struct {
	BuildID  string              `json:"build_id"`
	Name     string              `json:"name"`
	State    platform.BuildState `json:"state"`
	Created  time.Time           `json:"created"`
	Started  *time.Time          `json:"started,omitempty"`
	Finished *time.Time          `json:"finished,omitempty"`
	Log      []string            `json:"log"`
	LogFrom  int                 `json:"log_from"`
	LogLines int                 `json:"log_lines"`
	Runtime  *runtimeView        `json:"runtime,omitempty"`
	Error    string              `json:"error,omitempty"`
	// CollectorVersion is the manifest's release; verified is false for a
	// release newer than the support table.
	CollectorVersion string   `json:"collector_version"`
	Verified         bool     `json:"verified"`
	Warnings         []string `json:"warnings"`
	// Reused is true when the server took the binary of an earlier build of
	// identical inputs instead of building again.
	Reused bool `json:"reused"`
	// Image is the runtime's exported image by digest when the server
	// exports builds; export_error is why that export failed.
	Image       string `json:"image,omitempty"`
	ExportError string `json:"export_error,omitempty"`
}

// buildLogTail is how many log lines a result carries by default.
const buildLogTail = 50

func buildView(b *platform.RuntimeBuild) buildResult {
	out := buildResult{BuildID: b.ID, Name: b.Name, State: b.State, Created: b.Created, Log: b.Log, LogFrom: b.LogFrom,
		LogLines: b.LogLines, Runtime: runtimeOf(b.Runtime), Error: b.Error,
		CollectorVersion: b.CollectorVersion, Verified: b.Verified, Warnings: b.Warnings, Reused: b.Reused,
		Image: b.Image, ExportError: b.ExportError}
	if out.Warnings == nil {
		out.Warnings = []string{}
	}
	if !b.Started.IsZero() {
		out.Started = &b.Started
	}
	if !b.Finished.IsZero() {
		out.Finished = &b.Finished
	}
	if out.Log == nil {
		out.Log = []string{}
	}
	return out
}

// summary states the build's state and log counts, never its name, log or
// error text.
func (r buildResult) summary() string {
	text := "Runtime build: state " + string(r.State)
	if r.State == platform.BuildQueued || r.State == platform.BuildRunning {
		text += ", not finished within the wait"
	}
	text += fmt.Sprintf("; verified %t", r.Verified)
	if r.Reused {
		text += "; reused an earlier build"
	}
	switch {
	case r.Image != "":
		text += "; image exported"
	case r.ExportError != "":
		text += "; image export failed"
	}
	if len(r.Warnings) > 0 {
		text += "; " + plural(len(r.Warnings), "manifest warning")
	}
	return text + fmt.Sprintf("; %s returned of %d.", plural(len(r.Log), "log line"), r.LogLines)
}

func (in buildRuntimeInput) platform() (platform.BuildRuntimeRequest, int, error) {
	wait, err := waitSeconds(in.WaitSeconds)
	if err != nil {
		return platform.BuildRuntimeRequest{}, 0, err
	}
	if (in.Manifest == nil) == (in.ManifestYAML == nil) {
		return platform.BuildRuntimeRequest{}, 0, invalidRequest("supply exactly one of manifest or manifest_yaml")
	}
	req := platform.BuildRuntimeRequest{Name: in.Name}
	if in.ManifestYAML != nil {
		req.Manifest = platform.Source{Text: *in.ManifestYAML}
		return req, wait, nil
	}
	req.Manifest, err = in.Manifest.platform()
	return req, wait, err
}

// waitBuild observes a build for at most seconds, then reads its log from
// logFrom, or its last lines when logFrom is negative.
func waitBuild(ctx context.Context, service *platform.Service, id string, seconds, logFrom int) (*platform.RuntimeBuild, error) {
	waitCtx, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
	defer cancel()
	b, err := service.WaitRuntimeBuild(waitCtx, callerOf(waitCtx), id, max(logFrom, 0))
	if err != nil || logFrom >= 0 {
		return b, err
	}
	return service.GetRuntimeBuild(ctx, callerOf(ctx), id, max(b.LogLines-buildLogTail, 0))
}

type runtimeSourceInput struct {
	Module string `json:"module" jsonschema:"Module path the manifest names or replaces."`
	Git    string `json:"git,omitempty" jsonschema:"Remote git repository URL; the server reads no host directory or local repository."`
	Ref    string `json:"ref,omitempty" jsonschema:"Branch, tag or commit of the git repository; empty means its HEAD. The build records the commit."`
	Subdir string `json:"subdir,omitempty" jsonschema:"The module's directory inside the git repository."`
}

type composeManifestInput struct {
	Version       string               `json:"version,omitempty" jsonschema:"Verified Collector release, such as 0.162.0; empty is the newest verified one."`
	Preset        string               `json:"preset,omitempty" jsonschema:"all (every catalog component) or built-in (the built-in runtime's components)."`
	Configuration *source              `json:"configuration,omitempty" jsonschema:"Collector configuration whose components the manifest includes."`
	Components    []string             `json:"components,omitempty" jsonschema:"Catalog components to include as kind/type, such as processors/transform."`
	Sources       []runtimeSourceInput `json:"sources,omitempty" jsonschema:"Custom module sources: the manifest gets an entry for each component package found in them (a NewFactory returning a Collector factory). Build with the same sources (linea runtime build --source or --git)."`
}

type composedManifest struct {
	Manifest string   `json:"manifest"`
	Unknown  []string `json:"unknown"`
	Warnings []string `json:"warnings"`
	Custom   []string `json:"custom"`
}

// summary counts unknown components and warnings; the manifest stays in the
// structured result.
func (r composedManifest) summary() string {
	return fmt.Sprintf("Manifest written: %s from sources, %s not in the catalog, %s.", plural(len(r.Custom), "custom component"), plural(len(r.Unknown), "requested component"), plural(len(r.Warnings), "warning"))
}

func addRuntimeTools(server *sdk.Server, service *platform.Service) {
	addTool(server, "linea_list_runtimes", "List the built runtimes: Collector builds from an OCB manifest that runs, inspections and previews name in their runtime field to debug a specific Collector version and component set.",
		func(ctx context.Context, _ listRuntimesInput) (summarized, error) {
			list, err := service.ListRuntimes(ctx, callerOf(ctx))
			if err != nil {
				return nil, err
			}
			out := runtimesResult{Runtimes: []runtimeView{}}
			for i := range list {
				out.Runtimes = append(out.Runtimes, *runtimeOf(&list[i]))
			}
			return out, nil
		})
	addTool(server, "linea_build_runtime", "Queue a runtime build from an OCB manifest on the server's builders, for any Collector release from 0.160 on; a release newer than the debugger verified builds unverified. The manifest is checked before the build is queued; builds of identical inputs reuse the earlier binary. MCP sends no module sources: a manifest that builds a module from a local directory is refused (upload it with linea runtime build --server). Builds take minutes and never delay runs. Waiting only observes; read progress with linea_get_runtime_build.",
		func(ctx context.Context, in buildRuntimeInput) (summarized, error) {
			req, seconds, err := in.platform()
			if err != nil {
				return nil, err
			}
			b, err := service.BuildRuntime(ctx, callerOf(ctx), req)
			if err != nil {
				return nil, err
			}
			if b, err = waitBuild(ctx, service, b.ID, seconds, -1); err != nil {
				return nil, err
			}
			return buildView(b), nil
		})
	addTool(server, "linea_get_runtime_build", "Read a runtime build's state and log, optionally waiting for it to end. Waiting never cancels the build.",
		func(ctx context.Context, in getBuildInput) (summarized, error) {
			seconds := 0
			if in.WaitSeconds != nil {
				var err error
				if seconds, err = waitSeconds(in.WaitSeconds); err != nil {
					return nil, err
				}
			}
			from := -1
			if in.LogFrom != nil {
				if *in.LogFrom < 0 {
					return nil, invalidRequest("log_from must not be negative")
				}
				from = *in.LogFrom
			}
			b, err := waitBuild(ctx, service, in.BuildID, seconds, from)
			if err != nil {
				return nil, err
			}
			return buildView(b), nil
		})
	addTool(server, "linea_compose_manifest", "Write an OCB manifest from a verified Collector release's catalog: a preset, or the components a configuration defines plus picked ones. Custom components come from sources: each component package found in them gets an entry; build with the same sources. Requested types outside the catalog are listed as unknown. Nothing is saved or built.",
		func(ctx context.Context, in composeManifestInput) (summarized, error) {
			req := platform.ComposeManifestRequest{Version: in.Version, Preset: in.Preset, Components: in.Components}
			for _, s := range in.Sources {
				req.Sources = append(req.Sources, platform.SourceOverride{Module: s.Module, Git: s.Git, Ref: s.Ref, Subdir: s.Subdir})
			}
			if in.Configuration != nil {
				src, err := in.Configuration.platform()
				if err != nil {
					return nil, err
				}
				req.Configuration = &src
			}
			m, err := service.ComposeManifest(ctx, callerOf(ctx), req)
			if err != nil {
				return nil, err
			}
			out := composedManifest{Manifest: m.Manifest, Unknown: m.Unknown, Warnings: m.Warnings, Custom: m.Custom}
			if out.Custom == nil {
				out.Custom = []string{}
			}
			if out.Unknown == nil {
				out.Unknown = []string{}
			}
			if out.Warnings == nil {
				out.Warnings = []string{}
			}
			return out, nil
		})
}
