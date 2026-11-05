package mcp

import (
	"context"
	"fmt"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// Runtime management on MCP: everything HTTP and the
// CLI do with runtimes besides building and listing. Summaries count; names,
// image IDs and manifests stay in the structured result.

type runtimeNameInput struct {
	Name string `json:"name" jsonschema:"Runtime name, image reference or image ID from linea_list_runtimes."`
}

type runtimeResult struct {
	Runtime *runtimeView `json:"runtime"`
	// Manifest is the OCB manifest a built runtime was built from.
	Manifest string `json:"manifest,omitempty"`
}

func (r runtimeResult) summary() string {
	return fmt.Sprintf("Runtime: one built runtime; verified %t; %s.", r.Runtime.Verified, plural(len(r.Runtime.CustomModules), "custom module"))
}

type removedResult struct {
	Removed bool `json:"removed"`
}

func (removedResult) summary() string { return "Runtime removed." }

type pushRuntimeInput struct {
	Name   string `json:"name" jsonschema:"Runtime name, image reference or image ID from linea_list_runtimes."`
	Target string `json:"target" jsonschema:"Image reference to push to, for example registry.example.com/team/linea-runtime:0.162; uses the server's Docker credentials."`
}

type pushedResult struct {
	Pushed bool `json:"pushed"`
}

func (pushedResult) summary() string { return "Runtime pushed." }

type importRuntimeInput struct {
	Ref  string `json:"ref" jsonschema:"Image reference or ID of a runtime image."`
	Pull bool   `json:"pull,omitempty" jsonschema:"Pull the image first; otherwise it must already be in the server's Docker store."`
}

type defaultRuntimeInput struct {
	Name string `json:"name" jsonschema:"The built-in runtime (built-in) or a runtime of the catalog from linea_list_runtimes."`
}

type defaultRuntimeResult struct {
	Name string `json:"name"`
	// SetBy and Set say who chose the default runtime and when; absent until
	// the team chose one.
	SetBy string     `json:"set_by,omitempty"`
	Set   *time.Time `json:"set,omitempty"`
}

// summary states whether the team chose the default; its name stays in the
// structured result.
func (r defaultRuntimeResult) summary() string {
	if r.Set == nil {
		return "Default runtime: none chosen, the server's own."
	}
	return "Default runtime: chosen by the team."
}

type buildIDInput struct {
	BuildID string `json:"build_id" jsonschema:"Build ID from linea_build_runtime or linea_list_runtime_builds."`
}

type listBuildsInput struct {
	Limit  int    `json:"limit,omitempty" jsonschema:"Maximum builds, newest first; default 50, at most 500."`
	Cursor string `json:"cursor,omitempty" jsonschema:"next_cursor of the previous call, for the next older builds."`
}

type runtimeBuildsResult struct {
	Builds []buildResult `json:"builds"`
	// NextCursor continues the list with older builds; it is absent on the
	// last page.
	NextCursor string `json:"next_cursor,omitempty"`
}

func (r runtimeBuildsResult) summary() string {
	running := 0
	for _, b := range r.Builds {
		if b.State == platform.BuildQueued || b.State == platform.BuildRunning {
			running++
		}
	}
	return fmt.Sprintf("Runtime builds: %d listed, newest first; %d queued or running; more %t.", len(r.Builds), running, r.NextCursor != "")
}

type retargetManifestInput struct {
	Version      string  `json:"version,omitempty" jsonschema:"Verified Collector release to move to; empty is the newest verified one."`
	Manifest     *source `json:"manifest,omitempty" jsonschema:"OCB manifest source; supply exactly one of manifest or manifest_yaml."`
	ManifestYAML *string `json:"manifest_yaml,omitempty" jsonschema:"OCB manifest text; supply exactly one of manifest_yaml or manifest."`
}

type retargetedManifest struct {
	Manifest string   `json:"manifest"`
	From     string   `json:"from"`
	To       string   `json:"to"`
	Moved    []string `json:"moved"`
	Missing  []string `json:"missing"`
	Custom   []string `json:"custom"`
	Notes    []string `json:"notes"`
}

func (r retargetedManifest) summary() string {
	return fmt.Sprintf("Manifest retargeted: %s moved, %s missing from the release, %s kept.",
		plural(len(r.Moved), "module"), plural(len(r.Missing), "module"), plural(len(r.Custom), "custom module"))
}

type collectorSupport struct {
	Oldest   string   `json:"oldest"`
	Verified []string `json:"verified"`
	BuiltIn  string   `json:"built_in"`
}

func (r collectorSupport) summary() string {
	return fmt.Sprintf("Collector support: %s verified.", plural(len(r.Verified), "release"))
}

type catalogInput struct {
	Version string `json:"version,omitempty" jsonschema:"Verified Collector release; empty is the newest verified one."`
}

type catalogResult platform.CollectorCatalog

func (r catalogResult) summary() string {
	return fmt.Sprintf("Collector catalog: %s.", plural(len(r.Components), "component"))
}

type storedRuntime struct {
	runtimeView
	Size  int64  `json:"size"`
	State string `json:"state"`
	InUse bool   `json:"in_use"`
}

type cacheVolume struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

type leftoverContainer struct {
	Name    string `json:"name"`
	State   string `json:"state"`
	Running bool   `json:"running"`
}

type runtimeStorage struct {
	Runtimes    []storedRuntime     `json:"runtimes"`
	Caches      []cacheVolume       `json:"caches"`
	Leftovers   []leftoverContainer `json:"leftovers"`
	Reclaimable int64               `json:"reclaimable"`
}

func (r runtimeStorage) summary() string {
	return fmt.Sprintf("Runtime storage: %s, %s, %s; %d bytes reclaimable.",
		plural(len(r.Runtimes), "runtime image"), plural(len(r.Caches), "cache volume"), plural(len(r.Leftovers), "left-over container"), r.Reclaimable)
}

func storageOf(st *platform.RuntimeStorage) runtimeStorage {
	out := runtimeStorage{Runtimes: []storedRuntime{}, Caches: []cacheVolume{}, Leftovers: []leftoverContainer{}, Reclaimable: st.Reclaimable}
	for i := range st.Runtimes {
		r := &st.Runtimes[i]
		out.Runtimes = append(out.Runtimes, storedRuntime{runtimeView: *runtimeOf(&r.RuntimeInfo), Size: r.Size, State: string(r.State), InUse: r.InUse})
	}
	for _, v := range st.Caches {
		out.Caches = append(out.Caches, cacheVolume{Name: v.Name, Size: v.Size})
	}
	for _, c := range st.Leftovers {
		out.Leftovers = append(out.Leftovers, leftoverContainer{Name: c.Name, State: c.State, Running: c.Running})
	}
	return out
}

type pruneInput struct {
	Replaced bool `json:"replaced,omitempty" jsonschema:"Remove runtime images their name no longer selects."`
	Stale    bool `json:"stale,omitempty" jsonschema:"Remove runtimes of another worker protocol, named ones too."`
	Caches   bool `json:"caches,omitempty" jsonschema:"Remove the build cache volumes; the next build downloads its modules again. A conflict while a build is queued or running."`
}

type prunedItem struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	ImageID string `json:"image_id,omitempty"`
	Size    int64  `json:"size"`
	Reason  string `json:"reason,omitempty"`
}

type pruneResult struct {
	Removed   []prunedItem `json:"removed"`
	Kept      []prunedItem `json:"kept"`
	Reclaimed int64        `json:"reclaimed"`
}

func (r pruneResult) summary() string {
	return fmt.Sprintf("Runtime prune: %d removed, %d kept; %d bytes reclaimed.", len(r.Removed), len(r.Kept), r.Reclaimed)
}

func pruneOf(res *platform.PruneResult) pruneResult {
	items := func(list []platform.PrunedItem) []prunedItem {
		out := make([]prunedItem, 0, len(list))
		for _, i := range list {
			out = append(out, prunedItem{Kind: i.Kind, Name: i.Name, ImageID: i.ImageID, Size: i.Size, Reason: i.Reason})
		}
		return out
	}
	return pruneResult{Removed: items(res.Removed), Kept: items(res.Kept), Reclaimed: res.Reclaimed}
}

func orEmpty(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

func addRuntimeManagementTools(server *sdk.Server, service *platform.Service) {
	addTool(server, "linea_get_runtime", "Read one runtime: its Collector release, verification, worker protocol, custom modules and the manifest it was built from.",
		func(ctx context.Context, in runtimeNameInput) (summarized, error) {
			info, err := service.GetRuntime(ctx, callerOf(ctx), in.Name)
			if err != nil {
				return nil, err
			}
			return runtimeResult{Runtime: runtimeOf(info), Manifest: info.Manifest}, nil
		})
	addTool(server, "linea_get_default_runtime", "Read the default runtime: the runtime of every run, inspection and preview that names none, with who chose it and when. Until the team chose one it is the built-in runtime, or the one the server started with.",
		func(ctx context.Context, in struct{}) (summarized, error) {
			info, err := service.GetDefaultRuntime(ctx, callerOf(ctx))
			if err != nil {
				return nil, err
			}
			return defaultRuntimeResult{Name: info.Name, SetBy: info.SetBy, Set: info.Set}, nil
		})
	addTool(server, "linea_set_default_runtime", "Choose the default runtime of every server instance: the built-in runtime or a runtime of the catalog. Requests that name no runtime then run on it, and it cannot be removed until another is chosen.",
		func(ctx context.Context, in defaultRuntimeInput) (summarized, error) {
			info, err := service.SetDefaultRuntime(ctx, callerOf(ctx), in.Name)
			if err != nil {
				return nil, err
			}
			return defaultRuntimeResult{Name: info.Name, SetBy: info.SetBy, Set: info.Set}, nil
		})
	addTool(server, "linea_delete_runtime", "Remove a catalog runtime, or else a built runtime's image from the server's Docker store. Refused while it has queued or running runs.",
		func(ctx context.Context, in runtimeNameInput) (summarized, error) {
			if err := service.DeleteRuntime(ctx, callerOf(ctx), in.Name); err != nil {
				return nil, err
			}
			return removedResult{Removed: true}, nil
		})
	addTool(server, "linea_push_runtime", "Push a built runtime's image to a registry with the server's Docker credentials, for CI or another host.",
		func(ctx context.Context, in pushRuntimeInput) (summarized, error) {
			if err := service.PushRuntime(ctx, callerOf(ctx), platform.PushRuntimeRequest{Runtime: in.Name, Target: in.Target}); err != nil {
				return nil, err
			}
			return pushedResult{Pushed: true}, nil
		})
	addTool(server, "linea_import_runtime", "Accept a runtime image from the server's Docker store, or pull it first, after checking its labels and worker protocol.",
		func(ctx context.Context, in importRuntimeInput) (summarized, error) {
			info, err := service.ImportRuntime(ctx, callerOf(ctx), platform.ImportRuntimeRequest{Ref: in.Ref, Pull: in.Pull})
			if err != nil {
				return nil, err
			}
			return runtimeResult{Runtime: runtimeOf(info)}, nil
		})
	addTool(server, "linea_list_runtime_builds", "List the retained runtime builds newest first, without their logs: yours, or every build with runtime.manage. Pass next_cursor back for older ones.",
		func(ctx context.Context, in listBuildsInput) (summarized, error) {
			page, err := service.ListRuntimeBuilds(ctx, callerOf(ctx), platform.BuildListRequest{Limit: in.Limit, Cursor: in.Cursor})
			if err != nil {
				return nil, err
			}
			out := runtimeBuildsResult{Builds: make([]buildResult, 0, len(page.Builds)), NextCursor: page.NextCursor}
			for i := range page.Builds {
				out.Builds = append(out.Builds, buildView(&page.Builds[i]))
			}
			return out, nil
		})
	addTool(server, "linea_cancel_runtime_build", "Cancel a queued or running runtime build; a finished build is unchanged.",
		func(ctx context.Context, in buildIDInput) (summarized, error) {
			b, err := service.CancelRuntimeBuild(ctx, callerOf(ctx), in.BuildID)
			if err != nil {
				return nil, err
			}
			return buildView(b), nil
		})
	addTool(server, "linea_retarget_manifest", "Move an OCB manifest to another verified Collector release: core and contrib modules move, custom modules keep their version. Nothing is built.",
		func(ctx context.Context, in retargetManifestInput) (summarized, error) {
			if (in.Manifest == nil) == (in.ManifestYAML == nil) {
				return nil, invalidRequest("supply exactly one of manifest or manifest_yaml")
			}
			req := platform.RetargetManifestRequest{Version: in.Version}
			if in.ManifestYAML != nil {
				req.Manifest = platform.Source{Text: *in.ManifestYAML}
			} else {
				var err error
				if req.Manifest, err = in.Manifest.platform(); err != nil {
					return nil, err
				}
			}
			m, err := service.RetargetManifest(ctx, callerOf(ctx), req)
			if err != nil {
				return nil, err
			}
			return retargetedManifest{Manifest: m.Manifest, From: m.From, To: m.To, Moved: orEmpty(m.Moved), Missing: orEmpty(m.Missing),
				Custom: orEmpty(m.Custom), Notes: orEmpty(m.Notes)}, nil
		})
	addTool(server, "linea_get_collector_support", "Read the Collector releases runtimes can be built for: the oldest supported, the verified ones and the built-in runtime's.",
		func(ctx context.Context, in struct{}) (summarized, error) {
			v, err := service.GetCollectorSupport(ctx, callerOf(ctx))
			if err != nil {
				return nil, err
			}
			return collectorSupport{Oldest: v.Oldest, Verified: orEmpty(v.Verified), BuiltIn: v.BuiltIn}, nil
		})
	addTool(server, "linea_get_collector_catalog", "Read a verified Collector release's component catalog: each component's kind, type and module.",
		func(ctx context.Context, in catalogInput) (summarized, error) {
			cat, err := service.GetCollectorCatalog(ctx, callerOf(ctx), in.Version)
			if err != nil {
				return nil, err
			}
			return catalogResult(*cat), nil
		})
	addTool(server, "linea_get_runtime_storage", "Read what runtimes take on the server's Docker disk: runtime images with size and state, build cache volumes and left-over containers.",
		func(ctx context.Context, in struct{}) (summarized, error) {
			st, err := service.GetRuntimeStorage(ctx, callerOf(ctx))
			if err != nil {
				return nil, err
			}
			return storageOf(st), nil
		})
	addTool(server, "linea_prune_runtimes", "Remove replaced and, when asked, stale runtime images and the build caches, and always stopped build and worker containers. Images in use, tagged images and what Docker refuses are kept with the reason. Only the debugger's own objects are touched.",
		func(ctx context.Context, in pruneInput) (summarized, error) {
			res, err := service.PruneRuntimes(ctx, callerOf(ctx), platform.PruneRuntimesRequest{Replaced: in.Replaced, Stale: in.Stale, Caches: in.Caches})
			if err != nil {
				return nil, err
			}
			return pruneOf(res), nil
		})
}
