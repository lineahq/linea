package platform

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

// storageRuntimes adds unnamed images, cache volumes and left-over
// containers to fakeRuntimes.
type storageRuntimes struct {
	*fakeRuntimes
	extra     []RuntimeInfo // images their name does not select
	caches    []CacheVolume
	leftovers []LeftoverContainer
	// refuse makes Docker refuse removing these images or volumes.
	refuse map[string]bool
}

func (s *storageRuntimes) List(ctx context.Context) ([]RuntimeInfo, error) {
	list, _ := s.fakeRuntimes.List(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	return append(list, s.extra...), nil
}

func (s *storageRuntimes) Delete(ctx context.Context, info *RuntimeInfo) error {
	if s.refuse[info.ImageID] {
		return errors.New("image is being used by a stopped container")
	}
	s.mu.Lock()
	for i, e := range s.extra {
		if e.ImageID == info.ImageID {
			s.extra = slices.Delete(s.extra, i, i+1)
			s.mu.Unlock()
			return nil
		}
	}
	s.mu.Unlock()
	return s.fakeRuntimes.Delete(ctx, info)
}

func (s *storageRuntimes) CacheVolumes(context.Context) ([]CacheVolume, error) {
	return slices.Clone(s.caches), nil
}

func (s *storageRuntimes) RemoveCacheVolume(_ context.Context, name string) error {
	if s.refuse[name] {
		return errors.New("volume is in use")
	}
	s.caches = slices.DeleteFunc(s.caches, func(v CacheVolume) bool { return v.Name == name })
	return nil
}

func (s *storageRuntimes) Leftovers(context.Context) ([]LeftoverContainer, error) {
	return slices.Clone(s.leftovers), nil
}

func (s *storageRuntimes) RemoveLeftover(_ context.Context, c LeftoverContainer) error {
	s.leftovers = slices.DeleteFunc(s.leftovers, func(l LeftoverContainer) bool { return l.Name == c.Name })
	return nil
}

func newStorageRuntimes() *storageRuntimes {
	f := newFakeRuntimes("current", "busy")
	cur := f.images["current"]
	cur.Size = 70 << 20
	f.images["current"] = cur
	old := fakeInfo("current")
	old.ImageID, old.Size = "sha256:old-current", 69<<20
	pushed := fakeInfo("current")
	pushed.ImageID, pushed.Tags, pushed.Size = "sha256:pushed", []string{"registry.example.com/team/rt:1"}, 68<<20
	stale := fakeInfo("ancient")
	stale.ImageID, stale.Protocol, stale.Size = "sha256:ancient", 0, 60<<20
	return &storageRuntimes{
		fakeRuntimes: f,
		extra:        []RuntimeInfo{old, pushed, stale},
		caches:       []CacheVolume{{Name: "linea-gocache", Size: 900 << 20}, {Name: "linea-gomod", Size: 5 << 30}},
		leftovers: []LeftoverContainer{
			{Name: "linea-build-dead", State: "exited"},
			{Name: "linea-worker-live", State: "running", Running: true},
		},
		refuse: map[string]bool{},
	}
}

// TestRuntimeStorageNamesWhatCanGo reports every image with its state,
// counts only replaced and stale images not in use as reclaimable, and
// lists the caches and left-over containers.
func TestRuntimeStorageNamesWhatCanGo(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	store := newStorageRuntimes()
	store.execute = make(chan struct{})
	s := New(Options{Paths: PathPolicy{Unrestricted: true}, Runtimes: store})
	defer s.Close()
	run, err := s.SubmitRun(ctx, testCaller, RunRequest{Mode: ModeQuick, Config: Source{Path: logsCleanup("config.yaml")},
		Quick: &QuickInput{Fixture: Source{Path: logsCleanup("input.json")}}, Runtime: "busy"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		close(store.execute)
		_, _ = s.GetRunReport(ctx, testCaller, run.ID, true)
	}()
	st, err := s.GetRuntimeStorage(ctx, testCaller)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range st.Runtimes {
		state := string(r.State)
		if r.InUse {
			state += ",in use"
		}
		got[r.ImageID] = state
	}
	want := map[string]string{
		"sha256:image-current": "named", "sha256:image-busy": "named,in use",
		"sha256:old-current": "replaced", "sha256:pushed": "tagged", "sha256:ancient": "stale",
	}
	if mustJSON(t, got) != mustJSON(t, want) {
		t.Fatalf("states = %v, want %v", got, want)
	}
	if st.Reclaimable != 69<<20+60<<20 || len(st.Caches) != 2 || len(st.Leftovers) != 2 {
		t.Fatalf("storage = %+v", st)
	}
	// The runtime list leaves the replaced image to storage.
	list, err := s.ListRuntimes(ctx, testCaller)
	if err != nil || len(list) != 4 || slices.ContainsFunc(list, func(r RuntimeInfo) bool { return r.ImageID == "sha256:old-current" }) {
		t.Fatalf("runtimes = %+v, %v", list, err)
	}
}

// TestPruneRemovesOnlyWhatItWasAskedTo removes replaced images and stopped
// containers, keeps named, tagged and in-use images and running
// containers, keeps what Docker refuses with its reason, and removes the
// caches only when asked and no build is active.
func TestPruneRemovesOnlyWhatItWasAskedTo(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	store := newStorageRuntimes()
	s := New(Options{Runtimes: store})
	defer s.Close()

	res, err := s.PruneRuntimes(ctx, testCaller, PruneRuntimesRequest{Replaced: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Removed) != 2 || res.Removed[0].ImageID != "sha256:old-current" || res.Removed[1].Name != "linea-build-dead" ||
		len(res.Kept) != 0 || res.Reclaimed != 69<<20 {
		t.Fatalf("prune = %+v", res)
	}
	if len(store.leftovers) != 1 || !store.leftovers[0].Running || len(store.caches) != 2 {
		t.Fatalf("left = %+v, caches %+v", store.leftovers, store.caches)
	}
	if list, _ := s.ListRuntimes(ctx, testCaller); len(list) != 4 {
		t.Fatalf("runtimes after prune = %+v", list)
	}

	store.refuse["sha256:ancient"] = true
	res, err = s.PruneRuntimes(ctx, testCaller, PruneRuntimesRequest{Stale: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Removed) != 0 || len(res.Kept) != 1 || res.Kept[0].ImageID != "sha256:ancient" || res.Kept[0].Reason == "" {
		t.Fatalf("refused prune = %+v", res)
	}

	store.build = make(chan struct{})
	b, err := s.BuildRuntime(ctx, testCaller, BuildRuntimeRequest{Name: "next", Manifest: Source{Text: "dist:\n  otelcol_version: 0.162.0\n"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.PruneRuntimes(ctx, testCaller, PruneRuntimesRequest{Caches: true})
	wantCode(t, err, CodeConflict)
	close(store.build)
	if v, err := s.WaitRuntimeBuild(ctx, testCaller, b.ID, 0); err != nil || v.State != BuildDone {
		t.Fatalf("build = %+v, %v", v, err)
	}
	store.refuse["linea-gomod"] = true
	res, err = s.PruneRuntimes(ctx, testCaller, PruneRuntimesRequest{Caches: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Removed) != 1 || res.Removed[0].Name != "linea-gocache" || res.Reclaimed != 900<<20 ||
		len(res.Kept) != 1 || res.Kept[0].Name != "linea-gomod" {
		t.Fatalf("cache prune = %+v", res)
	}
}

// TestStorageWithoutDockerIsUnavailable keeps the built-in runtime's
// promise: no store, no runtime storage.
func TestStorageWithoutDockerIsUnavailable(t *testing.T) {
	s := New(Options{})
	defer s.Close()
	_, err := s.GetRuntimeStorage(context.Background(), testCaller)
	wantCode(t, err, CodeUnavailable)
	_, err = s.PruneRuntimes(context.Background(), testCaller, PruneRuntimesRequest{Replaced: true})
	wantCode(t, err, CodeUnavailable)
}
