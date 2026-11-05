package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// Runtime storage: runtime storage reports what runtimes take on
// Docker's disk and runtime prune removes what is no longer needed.

func runtimeStorageCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("runtime storage", "[--format text|json] [--server URL]", stderr)
	format := formatFlag(fs, "text")
	server := serverFlag(fs)
	if code, ok := parse(fs, args, format); !ok {
		return code
	}
	b, err := newRuntimeBackend(*server)
	if err != nil {
		return fail(stderr, err)
	}
	defer b.Close()
	st, err := b.Storage(ctx)
	if err != nil {
		return failRuntime(stderr, err)
	}
	if *format == "json" {
		err = writeIndented(stdout, st)
	} else {
		err = writeStorage(stdout, st)
	}
	if err != nil {
		return fail(stderr, err)
	}
	return exitOK
}

func writeIndented(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// writeStorage prints runtime images, caches and left-over containers.
func writeStorage(w io.Writer, st *api.RuntimeStorage) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "RUNTIME\tCOLLECTOR\tSTATE\tSIZE\tIMAGE")
	for _, r := range st.Runtimes {
		state := r.State
		if r.InUse {
			state += " (in use)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.Name, r.CollectorVersion, state, humanSize(r.Size), shortImageID(r.ImageID))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if len(st.Caches) > 0 {
		fmt.Fprintln(w)
		tw = tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "CACHE\tSIZE")
		for _, v := range st.Caches {
			fmt.Fprintf(tw, "%s\t%s\n", v.Name, humanSize(v.Size))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	for _, c := range st.Leftovers {
		fmt.Fprintf(w, "left-over container %s (%s)\n", c.Name, c.State)
	}
	_, err := fmt.Fprintf(w, "\nreclaimable by runtime prune: %s (replaced and stale images); caches only with --caches\n", humanSize(st.Reclaimable))
	return err
}

// humanSize prints bytes in decimal units, as Docker does.
func humanSize(n int64) string {
	if n < 0 {
		return "unknown"
	}
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "kMGTP"[exp])
}

func runtimePruneCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("runtime prune", "[--stale] [--caches] [--binaries [--runtime-cache DIR]] [--format text|json] [--server URL]", stderr)
	format := formatFlag(fs, "text")
	server := serverFlag(fs)
	stale := fs.Bool("stale", false, "also remove runtimes of another worker protocol, named ones too")
	caches := fs.Bool("caches", false, "also remove the build cache volumes: the next build downloads its modules again, and an offline build fails until it has")
	binaries := fs.Bool("binaries", false, "remove only the binaries of runtime images that direct commands cached, from --runtime-cache")
	runtimeCacheFlag(fs)
	if code, ok := parse(fs, args, format); !ok {
		return code
	}
	if *binaries {
		if *server != "" || *stale || *caches {
			fmt.Fprintln(stderr, "--binaries prunes this host's cache of runtime images; it takes no --server, --stale or --caches")
			fs.Usage()
			return exitError
		}
		n, err := platform.PruneImageCache(runtimeCache)
		if err != nil {
			return fail(stderr, err)
		}
		if *format == "json" {
			err = writeIndented(stdout, map[string]any{"directory": runtimeCache, "removed": n})
		} else {
			_, err = fmt.Fprintf(stdout, "removed %d runtime images from %s\n", n, runtimeCache)
		}
		if err != nil {
			return fail(stderr, err)
		}
		return exitOK
	}
	b, err := newRuntimeBackend(*server)
	if err != nil {
		return fail(stderr, err)
	}
	defer b.Close()
	res, err := b.Prune(ctx, api.RuntimePruneRequest{Replaced: true, Stale: *stale, Caches: *caches})
	if err != nil {
		return failRuntime(stderr, err)
	}
	if *format == "json" {
		err = writeIndented(stdout, res)
	} else {
		err = writePrune(stdout, res)
	}
	if err != nil {
		return fail(stderr, err)
	}
	return exitOK
}

func writePrune(w io.Writer, res *api.RuntimePruneResult) error {
	name := func(i api.PrunedItem) string {
		if i.Kind == "runtime" {
			return fmt.Sprintf("runtime %s (image %s)", i.Name, shortImageID(i.ImageID))
		}
		return i.Kind + " " + i.Name
	}
	for _, i := range res.Removed {
		fmt.Fprintf(w, "removed %s\n", name(i))
	}
	for _, i := range res.Kept {
		fmt.Fprintf(w, "kept %s: %s\n", name(i), i.Reason)
	}
	_, err := fmt.Fprintf(w, "reclaimed %s\n", humanSize(res.Reclaimed))
	return err
}

func (r connectedRuntimes) Storage(ctx context.Context) (*api.RuntimeStorage, error) {
	return r.c.RuntimeStorage(ctx)
}

func (r connectedRuntimes) Prune(ctx context.Context, req api.RuntimePruneRequest) (*api.RuntimePruneResult, error) {
	return r.c.PruneRuntimes(ctx, req)
}

func (r *localRuntimes) Storage(ctx context.Context) (*api.RuntimeStorage, error) {
	st, err := r.service.GetRuntimeStorage(ctx, cliCaller)
	if err != nil {
		return nil, err
	}
	out := &api.RuntimeStorage{Runtimes: []api.StoredRuntime{}, Caches: []api.CacheVolume{}, Leftovers: []api.LeftoverContainer{}, Reclaimable: st.Reclaimable}
	for i := range st.Runtimes {
		s := &st.Runtimes[i]
		out.Runtimes = append(out.Runtimes, api.StoredRuntime{Runtime: *apiRuntime(&s.RuntimeInfo), State: string(s.State), InUse: s.InUse})
	}
	for _, v := range st.Caches {
		out.Caches = append(out.Caches, api.CacheVolume{Name: v.Name, Size: v.Size})
	}
	for _, c := range st.Leftovers {
		out.Leftovers = append(out.Leftovers, api.LeftoverContainer{Name: c.Name, State: c.State, Running: c.Running})
	}
	return out, nil
}

func (r *localRuntimes) Prune(ctx context.Context, req api.RuntimePruneRequest) (*api.RuntimePruneResult, error) {
	res, err := r.service.PruneRuntimes(ctx, cliCaller, platform.PruneRuntimesRequest{Replaced: req.Replaced, Stale: req.Stale, Caches: req.Caches})
	if err != nil {
		return nil, err
	}
	items := func(list []platform.PrunedItem) []api.PrunedItem {
		out := make([]api.PrunedItem, 0, len(list))
		for _, i := range list {
			out = append(out, api.PrunedItem{Kind: i.Kind, Name: i.Name, ImageID: i.ImageID, Size: i.Size, Reason: i.Reason})
		}
		return out
	}
	return &api.RuntimePruneResult{Removed: items(res.Removed), Kept: items(res.Kept), Reclaimed: res.Reclaimed}, nil
}
