package platform

import (
	"context"
	"errors"
	"slices"

	"github.com/lineahq/linea/internal/debugger/authz"
	"github.com/lineahq/linea/internal/debugger/worker"
)

// Runtime storage: what built runtimes take on Docker's disk, and
// removing what is no longer needed. Only the debugger's own objects are
// touched: runtime images (by their label), its two build cache volumes and
// stopped build or worker containers. Nothing is removed implicitly.

// RuntimeHousekeeping is the part of a RuntimeStore that reports and removes
// build caches and left-over containers. A store without it has no storage
// to manage beyond its runtimes.
type RuntimeHousekeeping interface {
	CacheVolumes(ctx context.Context) ([]CacheVolume, error)
	RemoveCacheVolume(ctx context.Context, name string) error
	Leftovers(ctx context.Context) ([]LeftoverContainer, error)
	RemoveLeftover(ctx context.Context, c LeftoverContainer) error
}

// CacheVolume is a build cache volume: the Go module cache, which
// offline builds need, or the build cache.
type CacheVolume struct {
	Name string
	// Size is Docker's estimate in bytes, -1 when unknown.
	Size int64
}

// LeftoverContainer is a build or worker container Docker still holds.
// A running one may belong to another debugger process and is never
// removed.
type LeftoverContainer struct {
	Name    string
	State   string
	Running bool
}

// RuntimeImageState says why a runtime image is kept or can go.
type RuntimeImageState string

const (
	// RuntimeNamed is a runtime its name selects.
	RuntimeNamed RuntimeImageState = "named"
	// RuntimeReplaced lost its name to a rebuild under the same name and
	// has no other tag; only its image ID selects it (a recorded run on it
	// can still be repeated by image ID until it is removed).
	RuntimeReplaced RuntimeImageState = "replaced"
	// RuntimeTagged lost its name but another tag, such as a pushed or
	// imported reference, still names it; pruning never removes it.
	RuntimeTagged RuntimeImageState = "tagged"
	// RuntimeStale speaks another worker protocol: this debugger cannot run
	// it (unsupported_build).
	RuntimeStale RuntimeImageState = "stale"
)

// StoredRuntime is one runtime image with its size and state.
type StoredRuntime struct {
	RuntimeInfo
	State RuntimeImageState
	// InUse is true while this service has queued or running runs on it;
	// pruning keeps it.
	InUse bool
}

// RuntimeStorage is what the runtimes take on disk.
type RuntimeStorage struct {
	Runtimes  []StoredRuntime
	Caches    []CacheVolume
	Leftovers []LeftoverContainer
	// Reclaimable is the size of the replaced and stale images not in use;
	// stopped containers take next to nothing and caches are counted on
	// their own.
	Reclaimable int64
}

// PruneRuntimesRequest chooses what to remove. Stopped build and worker
// containers are always removed.
type PruneRuntimesRequest struct {
	// Replaced removes runtime images their name no longer selects.
	Replaced bool
	// Stale removes runtimes of another worker protocol, named ones too.
	Stale bool
	// Caches removes the build cache volumes: the next build downloads its
	// modules again, and an offline build fails until it has.
	Caches bool
}

// PrunedItem is one object a prune removed or kept.
type PrunedItem struct {
	// Kind is "runtime", "cache" or "container".
	Kind string
	Name string
	// ImageID names a runtime image.
	ImageID string
	Size    int64
	// Reason says why a selected object was kept.
	Reason string
}

// PruneResult lists what a prune removed and what it kept, with why.
type PruneResult struct {
	Removed   []PrunedItem
	Kept      []PrunedItem
	Reclaimed int64
}

// runtimeInUse reports whether this service has queued or running runs on
// the image.
func (s *Service) runtimeInUse(imageID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, job := range s.jobs {
		if job.runtime != nil && job.runtime.key() == imageID {
			return true
		}
	}
	return false
}

// stopIdleWorker closes the image's worker, if one is started.
func (s *Service) stopIdleWorker(imageID string) {
	s.runtimeMu.Lock()
	b := s.workers[imageID]
	if b != nil {
		delete(s.workers, imageID)
	}
	s.runtimeMu.Unlock()
	if b != nil {
		_ = b.worker.Close()
	}
}

// imageStates says, for each image, whether its name still selects it:
// the store's Get by name is the arbiter, so a rebuild under the same name
// or a removed name tag leaves the image replaced.
func imageStates(ctx context.Context, store RuntimeStore, list []RuntimeInfo) (map[string]RuntimeImageState, error) {
	selected := map[string]string{} // name -> image ID its name selects
	states := make(map[string]RuntimeImageState, len(list))
	for _, info := range list {
		if info.Protocol != worker.ProtocolVersion {
			states[info.ImageID] = RuntimeStale
			continue
		}
		id, ok := selected[info.Name]
		if !ok {
			got, err := store.Get(ctx, info.Name)
			switch {
			case err == nil:
				id = got.ImageID
			case !errors.Is(err, ErrRuntimeNotFound):
				return nil, err
			}
			selected[info.Name] = id
		}
		switch {
		case id == info.ImageID:
			states[info.ImageID] = RuntimeNamed
		case len(info.Tags) > 0:
			states[info.ImageID] = RuntimeTagged
		default:
			states[info.ImageID] = RuntimeReplaced
		}
	}
	return states, nil
}

// GetRuntimeStorage reports the runtime images, build caches and left-over
// containers with their sizes. Measuring the caches can take seconds.
func (s *Service) GetRuntimeStorage(ctx context.Context, caller Caller) (_ *RuntimeStorage, err error) {
	defer s.audit(&caller, "GetRuntimeStorage", &err)()
	if err := s.begin(ctx, caller, authz.RuntimeRead); err != nil {
		return nil, err
	}
	store, err := s.runtimeStore()
	if err != nil {
		return nil, err
	}
	list, err := store.List(ctx)
	if err != nil {
		return nil, classify(err, CodeInternal)
	}
	states, err := imageStates(ctx, store, list)
	if err != nil {
		return nil, classify(err, CodeInternal)
	}
	st := &RuntimeStorage{Runtimes: []StoredRuntime{}, Caches: []CacheVolume{}, Leftovers: []LeftoverContainer{}}
	for i := range list {
		r := StoredRuntime{RuntimeInfo: list[i], State: states[list[i].ImageID], InUse: s.runtimeInUse(list[i].ImageID)}
		if (r.State == RuntimeReplaced || r.State == RuntimeStale) && !r.InUse {
			st.Reclaimable += r.Size
		}
		st.Runtimes = append(st.Runtimes, r)
	}
	slices.SortFunc(st.Runtimes, func(a, b StoredRuntime) int {
		return cmpStrings(a.Name+"\x00"+string(a.State)+"\x00"+a.ImageID, b.Name+"\x00"+string(b.State)+"\x00"+b.ImageID)
	})
	if h, ok := store.(RuntimeHousekeeping); ok {
		if st.Caches, err = h.CacheVolumes(ctx); err != nil {
			return nil, classify(err, CodeInternal)
		}
		if st.Leftovers, err = h.Leftovers(ctx); err != nil {
			return nil, classify(err, CodeInternal)
		}
		if st.Caches == nil {
			st.Caches = []CacheVolume{}
		}
		if st.Leftovers == nil {
			st.Leftovers = []LeftoverContainer{}
		}
	}
	return st, nil
}

// PruneRuntimes removes the chosen runtime images, the build caches when
// asked, and stopped build and worker containers. Images with queued or
// running runs are kept; removing the caches while this service builds is
// a conflict. What Docker refuses (an image another tag or container still
// uses, a cache another process's build mounts) is kept with Docker's
// reason.
func (s *Service) PruneRuntimes(ctx context.Context, caller Caller, req PruneRuntimesRequest) (_ *PruneResult, err error) {
	defer s.audit(&caller, "PruneRuntimes", &err)()
	if err := s.begin(ctx, caller, authz.RuntimeManage); err != nil {
		return nil, err
	}
	store, err := s.runtimeStore()
	if err != nil {
		return nil, err
	}
	if req.Caches && s.builds.active() {
		return nil, &Error{Code: CodeConflict, Message: "a runtime build is queued or running and uses the build caches; wait for it or cancel it"}
	}
	res := &PruneResult{Removed: []PrunedItem{}, Kept: []PrunedItem{}}
	if req.Replaced || req.Stale {
		list, err := store.List(ctx)
		if err != nil {
			return nil, classify(err, CodeInternal)
		}
		states, err := imageStates(ctx, store, list)
		if err != nil {
			return nil, classify(err, CodeInternal)
		}
		for i := range list {
			info := &list[i]
			state := states[info.ImageID]
			if !(state == RuntimeReplaced && req.Replaced) && !(state == RuntimeStale && req.Stale) {
				continue
			}
			item := PrunedItem{Kind: "runtime", Name: info.Name, ImageID: info.ImageID, Size: info.Size}
			if s.runtimeInUse(info.ImageID) {
				item.Reason = "runs are queued or running on it"
				res.Kept = append(res.Kept, item)
				continue
			}
			s.stopIdleWorker(info.ImageID)
			if err := store.Delete(ctx, info); err != nil {
				item.Reason = err.Error()
				res.Kept = append(res.Kept, item)
				continue
			}
			res.Removed = append(res.Removed, item)
			res.Reclaimed += item.Size
		}
	}
	h, ok := store.(RuntimeHousekeeping)
	if !ok {
		return res, nil
	}
	leftovers, err := h.Leftovers(ctx)
	if err != nil {
		return nil, classify(err, CodeInternal)
	}
	for _, c := range leftovers {
		if c.Running {
			continue
		}
		item := PrunedItem{Kind: "container", Name: c.Name}
		if err := h.RemoveLeftover(ctx, c); err != nil {
			item.Reason = err.Error()
			res.Kept = append(res.Kept, item)
			continue
		}
		res.Removed = append(res.Removed, item)
	}
	if req.Caches {
		caches, err := h.CacheVolumes(ctx)
		if err != nil {
			return nil, classify(err, CodeInternal)
		}
		for _, v := range caches {
			item := PrunedItem{Kind: "cache", Name: v.Name, Size: v.Size}
			if err := h.RemoveCacheVolume(ctx, v.Name); err != nil {
				item.Reason = err.Error()
				res.Kept = append(res.Kept, item)
				continue
			}
			res.Removed = append(res.Removed, item)
			if v.Size > 0 {
				res.Reclaimed += v.Size
			}
		}
	}
	return res, nil
}
