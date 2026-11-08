package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/client"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// Built runtimes: debug Collectors built from OCB manifests and run
// in Docker. The runtime command builds, lists, removes, pushes and imports
// them, locally or on a team server with --server, and registers an image
// in a team server's catalog (runtime_register.go) and reads or chooses its
// default runtime (runtime_default.go).

// buildPollInterval is how often a followed build is read.
var buildPollInterval = time.Second

// cancelBuildTimeout bounds the cancellation sent after an interrupt.
const cancelBuildTimeout = 10 * time.Second

// runtimeBackend is the runtime operations, in-process or on a server.
// Building is not among them: a build runs on this machine's Docker
// (runtimeBuildCmd), or on a team server's builders (runtimeServerBuild).
type runtimeBackend interface {
	List(ctx context.Context) ([]api.Runtime, error)
	Remove(ctx context.Context, name string) error
	Push(ctx context.Context, name, target string) error
	Import(ctx context.Context, ref string, pull bool) (*api.Runtime, error)
	Storage(ctx context.Context) (*api.RuntimeStorage, error)
	Prune(ctx context.Context, req api.RuntimePruneRequest) (*api.RuntimePruneResult, error)
	Support(ctx context.Context) (*api.CollectorSupport, error)
	// Catalog reads a verified release's catalog; empty is the newest.
	Catalog(ctx context.Context, version string) (*platform.CollectorCatalog, error)
	// Compose writes a manifest; config is a path on this machine, or empty.
	Compose(ctx context.Context, req api.ManifestRequest, config string) (*api.ComposedManifest, error)
	// Retarget moves the manifest at a path on this machine.
	Retarget(ctx context.Context, req api.ManifestRetargetRequest, manifest string) (*api.RetargetedManifest, error)
	Close()
}

const runtimeSynopsis = "build --name NAME --manifest <ocb.yaml> [--source MODULE=DIR]... [--git MODULE=URL[#REF[:SUBDIR]]]... [--go-image IMG] [--module-proxy URL] [--offline] [--server URL]\n       linea runtime builds --server URL [--limit N] [--cursor C | --all] [--format text|json]\n       linea runtime list [--format text|json]\n       linea runtime versions [--format text|json]\n       linea runtime catalog [--version V] [--format text|json]\n       linea runtime manifest [--version V] [--preset all|built-in] [--config <collector.yaml>] [--component KIND/TYPE]... [--source MODULE=DIR]... [--git MODULE=URL[#REF[:SUBDIR]]]... [--out FILE]\n       linea runtime retarget --manifest <ocb.yaml> [--version V] [--out FILE]\n       linea runtime rm NAME\n       linea runtime push NAME TARGET\n       linea runtime import REF [--pull]\n       linea runtime register IMAGE --name NAME --server URL [--pull]\n       linea runtime default [NAME] --server URL [--format text|json]\n       linea runtime share NAME --server URL --projects P,Q|*|'' [--format text|json]\n       linea runtime storage [--format text|json]\n       linea runtime prune [--stale] [--caches] [--format text|json]\n       (every form takes [--server URL])"

func runtimeUsage(w io.Writer) {
	fmt.Fprintf(w, "usage: linea runtime %s\n", runtimeSynopsis)
}

// runtimeCmd dispatches the runtime subcommands.
func runtimeCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		runtimeUsage(stderr)
		return exitError
	}
	rest := args[1:]
	switch args[0] {
	case "help", "-h", "-help", "--help":
		runtimeUsage(stdout)
		return exitOK
	case "build":
		return runtimeBuildCmd(ctx, rest, stdout, stderr)
	case "list":
		return runtimeListCmd(ctx, rest, stdout, stderr)
	case "versions":
		return runtimeVersionsCmd(ctx, rest, stdout, stderr)
	case "catalog":
		return runtimeCatalogCmd(ctx, rest, stdout, stderr)
	case "builds":
		return runtimeBuildsCmd(ctx, rest, stdout, stderr)
	case "manifest":
		return runtimeManifestCmd(ctx, rest, stdout, stderr)
	case "retarget":
		return runtimeRetargetCmd(ctx, rest, stdout, stderr)
	case "storage":
		return runtimeStorageCmd(ctx, rest, stdout, stderr)
	case "prune":
		return runtimePruneCmd(ctx, rest, stdout, stderr)
	case "rm":
		return runtimeArgsCmd(ctx, "rm", "NAME", "[--server URL]", 1, nil, rest, stdout, stderr,
			func(ctx context.Context, b runtimeBackend, a []string) error {
				if err := b.Remove(ctx, a[0]); err != nil {
					return err
				}
				fmt.Fprintf(stdout, "removed runtime %s\n", a[0])
				return nil
			})
	case "push":
		return runtimeArgsCmd(ctx, "push", "NAME TARGET", "[--server URL]", 2, nil, rest, stdout, stderr,
			func(ctx context.Context, b runtimeBackend, a []string) error {
				if err := b.Push(ctx, a[0], a[1]); err != nil {
					return err
				}
				fmt.Fprintf(stdout, "pushed runtime %s as %s\n", a[0], a[1])
				return nil
			})
	case "default":
		return runtimeDefaultCmd(ctx, rest, stdout, stderr)
	case "share":
		return runtimeShareCmd(ctx, rest, stdout, stderr)
	case "register":
		return runtimeRegisterCmd(ctx, rest, stdout, stderr)
	case "import":
		var pull bool
		return runtimeArgsCmd(ctx, "import", "REF", "[--pull] [--server URL]", 1, func(fs *flag.FlagSet) {
			fs.BoolVar(&pull, "pull", false, "pull the image first; otherwise it must already be in Docker's store")
		}, rest, stdout, stderr,
			func(ctx context.Context, b runtimeBackend, a []string) error {
				rt, err := b.Import(ctx, a[0], pull)
				if err != nil {
					return err
				}
				fmt.Fprintf(stdout, "imported runtime %s (image %s, Collector %s)\n", rt.Name, shortImageID(rt.ImageID), rt.CollectorVersion)
				return nil
			})
	}
	fmt.Fprintf(stderr, "unknown runtime command %q\n", args[0])
	runtimeUsage(stderr)
	return exitError
}

// parseMixed parses flags and exactly want positional arguments, accepting
// flags after the positionals like the other commands do for --runs.
func parseMixed(fs *flag.FlagSet, args []string, want int) ([]string, int, bool) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, parseExit(err), false
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if want >= 0 && len(pos) != want {
		fmt.Fprintf(fs.Output(), "expected %d argument(s), got %d\n", want, len(pos))
		fs.Usage()
		return nil, exitError, false
	}
	return pos, exitOK, true
}

// runtimeArgsCmd runs a subcommand of positional arguments on a backend.
func runtimeArgsCmd(ctx context.Context, name, positional, flags string, want int, addFlags func(*flag.FlagSet), args []string, stdout, stderr io.Writer,
	do func(ctx context.Context, b runtimeBackend, args []string) error) int {
	fs := newFlagSet("runtime "+name, positional+" "+flags, stderr)
	server := serverFlag(fs)
	if addFlags != nil {
		addFlags(fs)
	}
	pos, code, ok := parseMixed(fs, args, want)
	if !ok {
		return code
	}
	b, err := newRuntimeBackend(*server)
	if err != nil {
		return fail(stderr, err)
	}
	defer b.Close()
	if err := do(ctx, b, pos); err != nil {
		return failRuntime(stderr, err)
	}
	return exitOK
}

func runtimeListCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("runtime list", "[--format text|json] [--server URL]", stderr)
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
	list, err := b.List(ctx)
	if err != nil {
		return failRuntime(stderr, err)
	}
	if list == nil {
		list = []api.Runtime{}
	}
	if *format == "json" {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		err = enc.Encode(list)
	} else {
		err = writeRuntimes(stdout, list)
	}
	if err != nil {
		return fail(stderr, err)
	}
	return exitOK
}

// writeRuntimes prints the runtime table.
func writeRuntimes(w io.Writer, list []api.Runtime) error {
	t := newTextOut(w)
	if len(list) == 0 {
		t.none("runtimes")
	}
	var rows [][]string
	for _, r := range list {
		image := shortImageID(r.ImageID)
		if r.Catalog != nil {
			// A catalog runtime names the image its hello was read from.
			image = r.Catalog.Image
		}
		if image == "" {
			image = "-"
		}
		name := r.Name
		if r.Default {
			name += " (default)"
		}
		rows = append(rows, []string{name, r.CollectorVersion, strconv.FormatBool(r.Verified), image})
	}
	t.table(0, []string{"name", "collector", "verified", "image"}, rows)
	return t.err()
}

// writeSupport prints which Collector releases this debugger verified.
func writeSupport(w io.Writer, support *api.CollectorSupport) error {
	t := newTextOut(w)
	t.field(0, "oldest release", support.Oldest)
	t.field(0, "verified releases", strings.Join(support.Verified, ", "))
	t.field(0, "built-in runtime", "Collector "+support.BuiltIn)
	t.line(0, "newer releases build unverified")
	return t.err()
}

func runtimeVersionsCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("runtime versions", "[--format text|json] [--server URL]", stderr)
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
	support, err := b.Support(ctx)
	if err != nil {
		return failRuntime(stderr, err)
	}
	if *format == "json" {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		err = enc.Encode(support)
	} else {
		err = writeSupport(stdout, support)
	}
	if err != nil {
		return fail(stderr, err)
	}
	return exitOK
}

func runtimeManifestCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("runtime manifest", "[--version V] [--preset all|built-in] [--config <collector.yaml>] [--component KIND/TYPE]... [--source MODULE=DIR]... [--git MODULE=URL[#REF[:SUBDIR]]]... [--out FILE] [--server URL]", stderr)
	var req api.ManifestRequest
	var components stringsFlag
	var sources sourceFlag
	fs.StringVar(&req.Version, "version", "", "verified Collector release; empty is the newest")
	fs.StringVar(&req.Preset, "preset", "", "every catalog component (all) or the built-in runtime's (built-in)")
	config := fs.String("config", "", "Collector configuration whose components the manifest includes")
	fs.Var(&components, "component", "a catalog component to include, such as processors/transform (repeatable)")
	fs.Var(&sources, "source", "a custom module's source directory, holding its go.mod; its components are added (repeatable)")
	fs.Var(gitFlag{&sources}, "git", "a custom module's git repository, cloned with this machine's git at REF (default HEAD), the module in SUBDIR; its components are added (repeatable)")
	out := fs.String("out", "", "write the manifest to this new file instead of stdout; never overwrites")
	server := serverFlag(fs)
	if code, ok := parse(fs, args, nil); !ok {
		return code
	}
	req.Components = components
	req.Sources = sources
	if *out != "" {
		if _, err := os.Stat(*out); err == nil {
			return fail(stderr, fmt.Errorf("--out %s exists; runtime manifest never overwrites a file", *out))
		}
	}
	b, err := newRuntimeBackend(*server)
	if err != nil {
		return fail(stderr, err)
	}
	defer b.Close()
	m, err := b.Compose(ctx, req, *config)
	if err != nil {
		return failRuntime(stderr, err)
	}
	for _, u := range m.Unknown {
		fmt.Fprintf(stderr, "warning: %s is not in the catalog; add its module to the manifest yourself\n", u)
	}
	for _, w := range m.Warnings {
		fmt.Fprintf(stderr, "warning: %s\n", w)
	}
	for _, c := range m.Custom {
		fmt.Fprintf(stderr, "added %s from its source; build with the same --source or --git\n", c)
	}
	if *out == "" {
		_, err = io.WriteString(stdout, m.Manifest)
	} else {
		err = writeNew(*out, m.Manifest)
	}
	if err != nil {
		return fail(stderr, err)
	}
	return exitOK
}

// runtimeRetargetCmd moves a manifest to another verified release:
// the core and contrib modules take the release's versions, the rest of the
// text stays as written.
func runtimeRetargetCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("runtime retarget", "--manifest <ocb.yaml> [--version V] [--out FILE] [--server URL]", stderr)
	var req api.ManifestRetargetRequest
	manifest := fs.String("manifest", "", "OpenTelemetry Collector Builder manifest YAML to move (read-only)")
	fs.StringVar(&req.Version, "version", "", "verified Collector release to move to; empty is the newest")
	out := fs.String("out", "", "write the manifest to this new file instead of stdout; never overwrites")
	server := serverFlag(fs)
	if code, ok := parse(fs, args, nil); !ok {
		return code
	}
	if *manifest == "" {
		fmt.Fprintln(stderr, "--manifest is required")
		fs.Usage()
		return exitError
	}
	if *out != "" {
		if _, err := os.Stat(*out); err == nil {
			return fail(stderr, fmt.Errorf("--out %s exists; runtime retarget never overwrites a file", *out))
		}
	}
	b, err := newRuntimeBackend(*server)
	if err != nil {
		return fail(stderr, err)
	}
	defer b.Close()
	m, err := b.Retarget(ctx, req, *manifest)
	if err != nil {
		return failRuntime(stderr, err)
	}
	fmt.Fprintf(stderr, "moved from %s to %s: %d module(s) changed version\n", m.From, m.To, len(m.Moved))
	for _, mod := range m.Missing {
		fmt.Fprintf(stderr, "warning: %s is not in release %s; replace or remove it before building\n", mod, m.To)
	}
	for _, mod := range m.Custom {
		fmt.Fprintf(stderr, "warning: custom module %s keeps its version; check it supports %s\n", mod, m.To)
	}
	for _, n := range m.Notes {
		fmt.Fprintf(stderr, "note: %s\n", n)
	}
	if *out == "" {
		_, err = io.WriteString(stdout, m.Manifest)
	} else {
		err = writeNew(*out, m.Manifest)
	}
	if err != nil {
		return fail(stderr, err)
	}
	return exitOK
}

// sourceFlag collects --source MODULE=DIR overrides.
type sourceFlag []api.RuntimeSource

func (s *sourceFlag) String() string { return "" }

func (s *sourceFlag) Set(v string) error {
	module, dir, ok := strings.Cut(v, "=")
	if !ok || module == "" || dir == "" {
		return errors.New("want MODULE=DIR")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	*s = append(*s, api.RuntimeSource{Module: module, Dir: abs})
	return nil
}

// gitFlag collects --git MODULE=URL[#REF[:SUBDIR]] overrides. A ref never
// contains ':' (git refuses it in ref names), so the first ':' after '#'
// starts the subdirectory.
type gitFlag struct{ s *sourceFlag }

func (g gitFlag) String() string { return "" }

func (g gitFlag) Set(v string) error {
	module, repo, ok := strings.Cut(v, "=")
	if !ok || module == "" || repo == "" {
		return errors.New("want MODULE=URL[#REF[:SUBDIR]]")
	}
	src := api.RuntimeSource{Module: module, Git: repo}
	if i := strings.LastIndex(repo, "#"); i >= 0 {
		src.Git = repo[:i]
		src.Ref, src.Subdir, _ = strings.Cut(repo[i+1:], ":")
	}
	if src.Git == "" {
		return errors.New("want MODULE=URL[#REF[:SUBDIR]]")
	}
	*g.s = append(*g.s, src)
	return nil
}

// shortImageID is the first 12 characters of an image ID after "sha256:".
func shortImageID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		id = id[:12]
	}
	return id
}

func runtimeBuildCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("runtime build", "--name NAME --manifest <ocb.yaml> [--source MODULE=DIR]... [--git MODULE=URL[#REF[:SUBDIR]]]... [--go-image IMG] [--module-proxy URL] [--offline] [--server URL]", stderr)
	var req platform.BuildRuntimeRequest
	var manifest string
	var sources sourceFlag
	fs.StringVar(&req.Name, "name", "", "runtime name: lower-case letters, digits, '.', '_' and '-'; building a name again replaces it")
	fs.StringVar(&manifest, "manifest", "", "OpenTelemetry Collector Builder manifest YAML; its relative path: and replace directories are read beside it")
	fs.Var(&sources, "source", "a module's source directory, holding its go.mod (repeatable); with --server it is packed and uploaded, replacing the manifest's directory for that module")
	fs.Var(gitFlag{&sources}, "git", "a module's git repository, cloned with this machine's git at REF (default HEAD), the module in SUBDIR (repeatable); not with --server")
	fs.StringVar(&req.GoImage, "go-image", "", "Go image to compile in; empty is the pinned official image; not with --server")
	fs.StringVar(&req.ModuleProxy, "module-proxy", "", "GOPROXY for this build; empty passes the module environment through; not with --server")
	fs.BoolVar(&req.Offline, "offline", false, "build from the module cache only, without network access; not with --server")
	server := serverFlag(fs)
	if code, ok := parse(fs, args, nil); !ok {
		return code
	}
	req.Sources = api.SourceOverrides(sources)
	if req.Name == "" || manifest == "" {
		fmt.Fprintln(stderr, "--name and --manifest are required")
		fs.Usage()
		return exitError
	}
	if *server != "" {
		return runtimeServerBuild(ctx, *server, req, manifest, sources, stdout, stderr)
	}
	req.Manifest = platform.Source{Path: manifest}
	r := &localRuntimes{service: platform.New(platform.Options{Paths: platform.PathPolicy{Unrestricted: true}, Runtimes: dockerRuntimes()})}
	defer r.Close()
	build, err := r.service.BuildRuntime(ctx, cliCaller, req)
	if err != nil {
		return failRuntime(stderr, err)
	}
	if !build.Verified {
		fmt.Fprintf(stderr, "note: Collector %s is newer than the releases this debugger verified; the runtime builds unverified\n", build.CollectorVersion)
	}
	return followBuild(ctx, r, build, stdout, stderr)
}

// buildReader reads and cancels runtime builds.
type buildReader interface {
	// GetBuild reads a build with its log lines from logFrom on.
	GetBuild(ctx context.Context, id string, logFrom int) (*platform.RuntimeBuild, error)
	CancelBuild(ctx context.Context, id string) error
}

func terminalBuild(s platform.BuildState) bool {
	return s == platform.BuildDone || s == platform.BuildFailed || s == platform.BuildCancelled
}

// followBuild prints the build's log as it grows until it ends. An interrupt
// cancels the build.
func followBuild(ctx context.Context, b buildReader, build *platform.RuntimeBuild, stdout, stderr io.Writer) int {
	from := 0
	cancelled := false
	tick := time.NewTicker(buildPollInterval)
	defer tick.Stop()
	for {
		// An interrupt, even one before the first read, cancels the build.
		if ctx.Err() != nil && !cancelled {
			cancelled = true
			cctx, stop := context.WithTimeout(context.WithoutCancel(ctx), cancelBuildTimeout)
			err := b.CancelBuild(cctx, build.ID)
			stop()
			if err != nil {
				return failRuntime(stderr, fmt.Errorf("interrupted; cancelling build %s failed: %w", build.ID, err))
			}
			// Read the cancelled build at once.
			tick.Reset(10 * time.Millisecond)
		}
		// After an interrupt the reads leave ctx, so the build's last
		// state is still printed.
		readCtx := ctx
		if cancelled {
			readCtx = context.WithoutCancel(ctx)
		}
		cur, err := b.GetBuild(readCtx, build.ID, from)
		if err != nil {
			if ctx.Err() != nil && !cancelled {
				continue // Interrupted during the read: cancel the build first.
			}
			return failRuntime(stderr, err)
		}
		for _, line := range cur.Log {
			fmt.Fprintln(stdout, line)
		}
		from = cur.LogFrom + len(cur.Log)
		if terminalBuild(cur.State) {
			return finishBuild(cur, stdout, stderr)
		}
		interrupted := ctx.Done()
		if cancelled {
			interrupted = nil // Already cancelling: only the poll tick wakes us.
		}
		select {
		case <-tick.C:
		case <-interrupted:
		}
	}
}

func finishBuild(b *platform.RuntimeBuild, stdout, stderr io.Writer) int {
	switch b.State {
	case platform.BuildDone:
		if rt := b.Runtime; rt != nil {
			verified := ""
			if !rt.Verified {
				verified = ", unverified"
			}
			fmt.Fprintf(stdout, "built runtime %s (image %s, Collector %s%s)\n", rt.Name, shortImageID(rt.ImageID), rt.CollectorVersion, verified)
			for _, m := range rt.CustomModules {
				commit := ""
				if m.Commit != "" {
					commit = " at commit " + m.Commit
				}
				fmt.Fprintf(stdout, "  custom module %s from %s%s\n", m.Module, m.Source, commit)
			}
			for _, p := range rt.Providers {
				fmt.Fprintf(stdout, "  provider %s from %s\n", cmp.Or(p.Name, "(named when the worker starts)"), p.Package)
			}
		} else if b.CollectorVersion != "" {
			// A server's builder registers the runtime itself, so the build
			// carries no image.
			notes := ""
			if !b.Verified {
				notes += ", unverified"
			}
			if b.Reused {
				notes += ", reused an earlier build of the same inputs"
			}
			fmt.Fprintf(stdout, "built runtime %s (Collector %s%s)\n", b.Name, b.CollectorVersion, notes)
		} else {
			fmt.Fprintf(stdout, "built runtime %s\n", b.Name)
		}
		// The server's export never fails the build.
		if b.Image != "" {
			fmt.Fprintf(stdout, "exported image %s\n", b.Image)
		}
		if b.ExportError != "" {
			fmt.Fprintf(stderr, "warning: the runtime is built, but exporting its image failed: %s\n", b.ExportError)
		}
		return exitOK
	case platform.BuildCancelled:
		fmt.Fprintf(stderr, "error: build of runtime %s was cancelled\n", b.Name)
	default:
		msg := b.Error
		if msg == "" {
			msg = "the build failed"
		}
		fmt.Fprintf(stderr, "error: build of runtime %s failed: %s\n", b.Name, msg)
	}
	return exitError
}

// failRuntime prints the error with the diagnostics of a refusal.
func failRuntime(stderr io.Writer, err error) int {
	code := fail(stderr, err)
	var diags []platform.Diagnostic
	var pe *platform.Error
	var ce *client.Error
	switch {
	case errors.As(err, &pe):
		diags = pe.Diagnostics
	case errors.As(err, &ce):
		diags = ce.Diagnostics
	}
	for _, d := range diags {
		fmt.Fprintf(stderr, "  %s %s", d.Severity, d.Code)
		if d.Component != "" {
			fmt.Fprintf(stderr, " [%s]", d.Component)
		}
		fmt.Fprintf(stderr, ": %s\n", d.Message)
		if d.Hint != "" {
			fmt.Fprintf(stderr, "    hint: %s\n", d.Hint)
		}
	}
	return code
}

func newRuntimeBackend(server string) (runtimeBackend, error) {
	if server != "" {
		c, err := connect(server, false)
		if err != nil {
			return nil, err
		}
		return connectedRuntimes{c}, nil
	}
	return &localRuntimes{service: platform.New(platform.Options{Paths: platform.PathPolicy{Unrestricted: true}, Runtimes: dockerRuntimes()})}, nil
}

// connectedRuntimes operates on a team server.
type connectedRuntimes struct{ c *client.Client }

func (connectedRuntimes) Close() {}

func (r connectedRuntimes) Support(ctx context.Context) (*api.CollectorSupport, error) {
	return r.c.CollectorSupport(ctx)
}

// Compose sends the configuration as text. A team server reads no host
// directory, so it takes git sources only.
func (r connectedRuntimes) Compose(ctx context.Context, req api.ManifestRequest, config string) (*api.ComposedManifest, error) {
	for _, s := range req.Sources {
		if s.Dir != "" {
			return nil, fmt.Errorf("--source %s=%s cannot be used with --server: a team server reads no host directory; pass a repository it can clone with --git", s.Module, s.Dir)
		}
	}
	if config != "" {
		text, err := readArg("config", config)
		if err != nil {
			return nil, err
		}
		req.ConfigYAML = text
	}
	return r.c.ComposeManifest(ctx, req)
}

func (r connectedRuntimes) Retarget(ctx context.Context, req api.ManifestRetargetRequest, manifest string) (*api.RetargetedManifest, error) {
	text, err := readArg("manifest", manifest)
	if err != nil {
		return nil, err
	}
	req.ManifestYAML = text
	return r.c.RetargetManifest(ctx, req)
}

func (r connectedRuntimes) List(ctx context.Context) ([]api.Runtime, error) {
	return r.c.ListRuntimes(ctx)
}

func (r connectedRuntimes) Remove(ctx context.Context, name string) error {
	return r.c.DeleteRuntime(ctx, name)
}

func (r connectedRuntimes) Push(ctx context.Context, name, target string) error {
	return r.c.PushRuntime(ctx, name, target)
}

func (r connectedRuntimes) Import(ctx context.Context, ref string, pull bool) (*api.Runtime, error) {
	return r.c.ImportRuntime(ctx, ref, pull)
}

// localRuntimes operates on an in-process platform and its Docker store.
type localRuntimes struct{ service *platform.Service }

func (r *localRuntimes) Close() { r.service.Close() }

func apiRuntime(v *platform.RuntimeInfo) *api.Runtime {
	if v == nil {
		return nil
	}
	tags := v.Tags
	if tags == nil {
		tags = []string{}
	}
	return &api.Runtime{Name: v.Name, ImageID: v.ImageID, Tags: tags, CollectorVersion: v.CollectorVersion, ProductVersion: v.ProductVersion,
		Protocol: v.Protocol, Verified: v.Verified, Default: v.Default, Manifest: v.Manifest, ModuleSet: v.ModuleSet, GoImage: v.GoImage,
		CustomModules: orEmpty(v.CustomModules), Size: v.Size, Providers: v.Providers}
}

func orEmpty[T any](list []T) []T {
	if list == nil {
		return []T{}
	}
	return list
}

func (r *localRuntimes) Support(ctx context.Context) (*api.CollectorSupport, error) {
	v, err := r.service.GetCollectorSupport(ctx, cliCaller)
	if err != nil {
		return nil, err
	}
	return &api.CollectorSupport{Oldest: v.Oldest, Verified: orEmpty(v.Verified), BuiltIn: v.BuiltIn}, nil
}

func (r *localRuntimes) Compose(ctx context.Context, req api.ManifestRequest, config string) (*api.ComposedManifest, error) {
	in := platform.ComposeManifestRequest{Version: req.Version, Preset: req.Preset, Components: req.Components, Sources: api.SourceOverrides(req.Sources)}
	if config != "" {
		in.Configuration = &platform.Source{Path: config}
	}
	m, err := r.service.ComposeManifest(ctx, cliCaller, in)
	if err != nil {
		return nil, err
	}
	return &api.ComposedManifest{Manifest: m.Manifest, Unknown: orEmpty(m.Unknown), Warnings: orEmpty(m.Warnings), Custom: orEmpty(m.Custom), Providers: orEmpty(m.Providers)}, nil
}

func (r *localRuntimes) Retarget(ctx context.Context, req api.ManifestRetargetRequest, manifest string) (*api.RetargetedManifest, error) {
	m, err := r.service.RetargetManifest(ctx, cliCaller, platform.RetargetManifestRequest{Version: req.Version, Manifest: platform.Source{Path: manifest}})
	if err != nil {
		return nil, err
	}
	return &api.RetargetedManifest{Manifest: m.Manifest, From: m.From, To: m.To, Moved: orEmpty(m.Moved), Missing: orEmpty(m.Missing),
		Custom: orEmpty(m.Custom), Notes: orEmpty(m.Notes)}, nil
}

func (r *localRuntimes) GetBuild(ctx context.Context, id string, from int) (*platform.RuntimeBuild, error) {
	return r.service.GetRuntimeBuild(ctx, cliCaller, id, from)
}

func (r *localRuntimes) CancelBuild(ctx context.Context, id string) error {
	_, err := r.service.CancelRuntimeBuild(ctx, cliCaller, id)
	return err
}

func (r *localRuntimes) List(ctx context.Context) ([]api.Runtime, error) {
	list, err := r.service.ListRuntimes(ctx, cliCaller)
	if err != nil {
		return nil, err
	}
	out := make([]api.Runtime, 0, len(list))
	for i := range list {
		out = append(out, *apiRuntime(&list[i]))
	}
	return out, nil
}

func (r *localRuntimes) Remove(ctx context.Context, name string) error {
	return r.service.DeleteRuntime(ctx, cliCaller, name)
}

func (r *localRuntimes) Push(ctx context.Context, name, target string) error {
	return r.service.PushRuntime(ctx, cliCaller, platform.PushRuntimeRequest{Runtime: name, Target: target})
}

func (r *localRuntimes) Import(ctx context.Context, ref string, pull bool) (*api.Runtime, error) {
	info, err := r.service.ImportRuntime(ctx, cliCaller, platform.ImportRuntimeRequest{Ref: ref, Pull: pull})
	if err != nil {
		return nil, err
	}
	return apiRuntime(info), nil
}
