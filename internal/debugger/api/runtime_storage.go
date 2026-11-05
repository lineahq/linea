package api

import (
	"net/http"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// Runtime storage: what runtimes take on the server's Docker disk,
// and pruning what is no longer needed. Only the debugger's runtime
// images, its two build cache volumes and stopped build or worker
// containers are ever touched.

// StoredRuntime is a runtime image with its state: named (its name
// selects it), replaced (a rebuild took its name; only its image ID
// selects it), tagged (only another tag, such as a pushed reference, names
// it) or stale (another worker protocol; this debugger cannot run it).
type StoredRuntime struct {
	Runtime
	State string `json:"state"`
	// InUse is true while the server has queued or running runs on it.
	InUse bool `json:"in_use"`
}

// CacheVolume is a build cache volume: linea-gomod (the module
// cache offline builds need) or linea-gocache (the build cache).
type CacheVolume struct {
	Name string `json:"name"`
	// Size is Docker's estimate in bytes, -1 when unknown.
	Size int64 `json:"size"`
}

// LeftoverContainer is a build or worker container Docker still holds; a
// running one may belong to another debugger process and is never removed.
type LeftoverContainer struct {
	Name    string `json:"name"`
	State   string `json:"state"`
	Running bool   `json:"running"`
}

// RuntimeStorage is what runtimes take on disk; reclaimable counts the
// replaced and stale images not in use.
type RuntimeStorage struct {
	Runtimes    []StoredRuntime     `json:"runtimes"`
	Caches      []CacheVolume       `json:"caches"`
	Leftovers   []LeftoverContainer `json:"leftovers"`
	Reclaimable int64               `json:"reclaimable"`
}

// RuntimePruneRequest chooses what to remove; stopped build and worker
// containers are always removed. caches makes the next build download its
// modules again, and an offline build fails until it has.
type RuntimePruneRequest struct {
	Replaced bool `json:"replaced,omitempty"`
	Stale    bool `json:"stale,omitempty"`
	Caches   bool `json:"caches,omitempty"`
}

// PrunedItem is an object a prune removed, or kept with the reason.
type PrunedItem struct {
	// Kind is runtime, cache or container.
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	ImageID string `json:"image_id,omitempty"`
	Size    int64  `json:"size"`
	Reason  string `json:"reason,omitempty"`
}

// RuntimePruneResult lists what a prune removed and kept.
type RuntimePruneResult struct {
	Removed   []PrunedItem `json:"removed"`
	Kept      []PrunedItem `json:"kept"`
	Reclaimed int64        `json:"reclaimed"`
}

func (s *Server) getRuntimeStorage(w http.ResponseWriter, r *http.Request) {
	st, err := s.runs.GetRuntimeStorage(r.Context(), callerOf(r))
	if err != nil {
		writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, runtimeStorageOf(st))
}

func runtimeStorageOf(st *platform.RuntimeStorage) RuntimeStorage {
	out := RuntimeStorage{Runtimes: []StoredRuntime{}, Caches: []CacheVolume{}, Leftovers: []LeftoverContainer{}, Reclaimable: st.Reclaimable}
	for i := range st.Runtimes {
		r := &st.Runtimes[i]
		out.Runtimes = append(out.Runtimes, StoredRuntime{Runtime: *runtimeOf(&r.RuntimeInfo), State: string(r.State), InUse: r.InUse})
	}
	for _, v := range st.Caches {
		out.Caches = append(out.Caches, CacheVolume{Name: v.Name, Size: v.Size})
	}
	for _, c := range st.Leftovers {
		out.Leftovers = append(out.Leftovers, LeftoverContainer{Name: c.Name, State: c.State, Running: c.Running})
	}
	return out
}

func (s *Server) pruneRuntimes(w http.ResponseWriter, r *http.Request) {
	var req RuntimePruneRequest
	if !decode(w, r, &req) {
		return
	}
	res, err := s.runs.PruneRuntimes(r.Context(), callerOf(r), platform.PruneRuntimesRequest{Replaced: req.Replaced, Stale: req.Stale, Caches: req.Caches})
	if err != nil {
		writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pruneResultOf(res))
}

func pruneResultOf(res *platform.PruneResult) RuntimePruneResult {
	items := func(list []platform.PrunedItem) []PrunedItem {
		out := make([]PrunedItem, 0, len(list))
		for _, i := range list {
			out = append(out, PrunedItem{Kind: i.Kind, Name: i.Name, ImageID: i.ImageID, Size: i.Size, Reason: i.Reason})
		}
		return out
	}
	return RuntimePruneResult{Removed: items(res.Removed), Kept: items(res.Kept), Reclaimed: res.Reclaimed}
}

func runtimeStoragePaths() map[string]any {
	return map[string]any{
		"/api/v1/runtime-storage": map[string]any{"get": op("getRuntimeStorage",
			"What runtimes take on the server's Docker disk: every runtime image with its size and state, the build cache volumes with Docker's size estimate (measuring them can take seconds) and left-over build or worker containers.",
			nil, "", map[string]any{
				"200": reply("The storage.", ref("RuntimeStorage")),
				"503": operationFailure(noDocker),
			})},
		"/api/v1/runtime-storage/prune": map[string]any{"post": op("pruneRuntimes",
			"Remove replaced and, when asked, stale runtime images, the build cache volumes when asked, and always stopped build and worker containers. Images with queued or running runs and objects Docker refuses to remove are kept with the reason. Named and tagged runtimes are never removed except stale ones with stale.",
			nil, "RuntimePruneRequest", map[string]any{
				"200": reply("What was removed and kept.", ref("RuntimePruneResult")),
				"400": operationFailure("Malformed request."),
				"409": operationFailure("caches was asked while a runtime build is queued or running."),
				"503": operationFailure(noDocker),
			})},
	}
}
