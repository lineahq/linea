package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/client"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// Server runtime builds: runtime build --server packs the
// module directories the manifest uses, uploads them with the manifest and
// follows the build a builder runs; runtime builds lists a server's builds.
// Release catalogs: runtime catalog prints a verified
// release's components.

// runtimeServerBuild queues a build on a team server and follows it. The
// manifest is sent as text; each module directory it builds from, and each
// --source directory, is packed and uploaded. Settings the server's builders
// own are refused here rather than ignored.
func runtimeServerBuild(ctx context.Context, server string, req platform.BuildRuntimeRequest, manifestPath string, sources sourceFlag, stdout, stderr io.Writer) int {
	if req.GoImage != "" || req.ModuleProxy != "" || req.Offline {
		fmt.Fprintln(stderr, "--go-image, --module-proxy and --offline cannot be used with --server: a server's builders choose how they compile")
		return exitError
	}
	overrides := map[string]string{}
	for _, s := range sources {
		if s.Git != "" {
			fmt.Fprintf(stderr, "--git cannot be used with --server: clone %s and pass the directory with --source %s=DIR\n", s.Git, s.Module)
			return exitError
		}
		overrides[s.Module] = s.Dir
	}
	text, err := os.ReadFile(manifestPath)
	if err != nil {
		return fail(stderr, fmt.Errorf("--manifest: %w", err))
	}
	modules, err := buildLocalModules(text, manifestPath, overrides)
	if err != nil {
		return fail(stderr, err)
	}
	archives := make([]client.BuildArchive, 0, len(modules))
	for _, m := range modules {
		data, files, err := packModule(m.Dir)
		if err != nil {
			return fail(stderr, fmt.Errorf("the source of %s: %w", m.Module, err))
		}
		fmt.Fprintf(stderr, "packed %s from %s: %d files, %d bytes\n", m.Module, shortPath(m.Dir), files, len(data))
		archives = append(archives, client.BuildArchive{Module: m.Module, Data: data})
	}
	c, err := connect(server, false)
	if err != nil {
		return fail(stderr, err)
	}
	queued, err := c.BuildRuntime(ctx, api.RuntimeBuildRequest{Name: req.Name, ManifestYAML: string(text)}, archives)
	if err != nil {
		return failRuntime(stderr, err)
	}
	if !queued.Verified {
		fmt.Fprintf(stderr, "note: Collector %s is newer than the releases this debugger verified; the runtime builds unverified\n", queued.CollectorVersion)
	}
	if queued.Reused {
		fmt.Fprintln(stderr, "note: this server built identical inputs before; the earlier build's binary is reused")
	}
	for _, w := range queued.Warnings {
		fmt.Fprintf(stderr, "warning: %s\n", w)
	}
	fmt.Fprintf(stderr, "build %s queued\n", queued.ID)
	return followBuild(ctx, connectedBuilds{c}, &platform.RuntimeBuild{ID: queued.ID}, stdout, stderr)
}

// shortPath names a directory relative to the working directory when it is
// inside it.
func shortPath(dir string) string {
	if wd, err := os.Getwd(); err == nil {
		if rel, err := filepath.Rel(wd, dir); err == nil && !strings.HasPrefix(rel, "..") {
			return rel
		}
	}
	return dir
}

// connectedBuilds reads and cancels a team server's builds as followBuild
// takes them.
type connectedBuilds struct{ c *client.Client }

func (b connectedBuilds) GetBuild(ctx context.Context, id string, from int) (*platform.RuntimeBuild, error) {
	v, err := b.c.GetRuntimeBuild(ctx, id, from)
	if err != nil {
		return nil, err
	}
	out := &platform.RuntimeBuild{ID: v.ID, Name: v.Name, State: platform.BuildState(v.State), Created: v.Created, Log: v.Log, LogFrom: v.LogFrom,
		LogLines: v.LogLines, Error: v.Error, CollectorVersion: v.CollectorVersion, Verified: v.Verified, Warnings: v.Warnings, Reused: v.Reused,
		Image: v.Image, ExportError: v.ExportError}
	if rt := v.Runtime; rt != nil {
		out.Runtime = &platform.RuntimeInfo{Name: rt.Name, ImageID: rt.ImageID, CollectorVersion: rt.CollectorVersion, Verified: rt.Verified,
			CustomModules: rt.CustomModules, Providers: rt.Providers}
	}
	if v.Started != nil {
		out.Started = *v.Started
	}
	if v.Finished != nil {
		out.Finished = *v.Finished
	}
	return out, nil
}

func (b connectedBuilds) CancelBuild(ctx context.Context, id string) error {
	_, err := b.c.CancelRuntimeBuild(ctx, id)
	return err
}

// runtimeBuildsCmd lists a team server's builds, newest first, page by page
// like runs.
func runtimeBuildsCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("runtime builds", "--server URL [--limit N] [--cursor C | --all] [--format text|json]", stderr)
	format := formatFlag(fs, "text")
	server := serverFlag(fs)
	limit := fs.Int("limit", 0, "builds per page (default 50, at most 500)")
	cursor := fs.String("cursor", "", "continue after the page that printed this cursor")
	all := fs.Bool("all", false, "follow every page to the oldest build")
	if code, ok := parse(fs, args, format); !ok {
		return code
	}
	if *all && *cursor != "" {
		fmt.Fprintln(stderr, "--all and --cursor cannot be combined")
		fs.Usage()
		return exitError
	}
	if *server == "" {
		fmt.Fprintln(stderr, "runtime builds needs --server: builds are retained by a team server")
		return exitError
	}
	c, err := connect(*server, false)
	if err != nil {
		return fail(stderr, err)
	}
	page, err := c.ListRuntimeBuilds(ctx, *limit, *cursor)
	for err == nil && *all && page.NextCursor != "" {
		var next *api.RuntimeBuildPage
		if next, err = c.ListRuntimeBuilds(ctx, *limit, page.NextCursor); err == nil {
			next.Builds = append(page.Builds, next.Builds...)
			page = next
		}
	}
	if err != nil {
		return failRuntime(stderr, err)
	}
	if page.Builds == nil {
		page.Builds = []api.RuntimeBuild{}
	}
	if *format == "json" {
		err = writeIndented(stdout, page)
	} else {
		err = writeBuilds(stdout, page.Builds)
		if err == nil && page.NextCursor != "" {
			fmt.Fprintf(stderr, "more builds: pass --cursor %s, or --all\n", page.NextCursor)
		}
	}
	if err != nil {
		return fail(stderr, err)
	}
	return exitOK
}

func writeBuilds(w io.Writer, list []api.RuntimeBuild) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "BUILD\tNAME\tSTATE\tCOLLECTOR\tREUSED\tCREATED")
	for _, b := range list {
		reused := "-"
		if b.Reused {
			reused = "yes"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", b.ID, b.Name, b.State, b.CollectorVersion, reused, b.Created.Local().Format(time.DateTime))
	}
	return tw.Flush()
}

func runtimeCatalogCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("runtime catalog", "[--version V] [--format text|json] [--server URL]", stderr)
	format := formatFlag(fs, "text")
	server := serverFlag(fs)
	version := fs.String("version", "", "verified Collector release; empty is the newest")
	if code, ok := parse(fs, args, format); !ok {
		return code
	}
	b, err := newRuntimeBackend(*server)
	if err != nil {
		return fail(stderr, err)
	}
	defer b.Close()
	cat, err := b.Catalog(ctx, *version)
	if err != nil {
		return failRuntime(stderr, err)
	}
	if *format == "json" {
		err = writeIndented(stdout, cat)
	} else {
		err = writeCatalog(stdout, cat)
	}
	if err != nil {
		return fail(stderr, err)
	}
	return exitOK
}

func writeCatalog(w io.Writer, cat *platform.CollectorCatalog) error {
	fmt.Fprintf(w, "Collector %s: %d components\n\n", cat.CollectorVersion, len(cat.Components))
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "KIND\tTYPE\tMODULE")
	for _, c := range cat.Components {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", c.Kind, c.Type, c.GoMod)
	}
	return tw.Flush()
}

func (r connectedRuntimes) Catalog(ctx context.Context, version string) (*platform.CollectorCatalog, error) {
	if version == "" {
		support, err := r.c.CollectorSupport(ctx)
		if err != nil {
			return nil, err
		}
		if len(support.Verified) == 0 {
			return nil, &platform.Error{Code: platform.CodeNotFound, Message: "the server verified no Collector release"}
		}
		version = support.Verified[len(support.Verified)-1]
	}
	return r.c.CollectorCatalog(ctx, version)
}

func (r *localRuntimes) Catalog(ctx context.Context, version string) (*platform.CollectorCatalog, error) {
	return r.service.GetCollectorCatalog(ctx, cliCaller, version)
}
