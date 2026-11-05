package platform

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing/fstest"
	"time"

	"github.com/lineahq/linea/build/versions"
	"github.com/lineahq/linea/internal/debugger/catalog"
	"github.com/lineahq/linea/internal/debugger/hostfs"
	"github.com/lineahq/linea/internal/debugger/manifest"
)

// Custom component sources: a build may supply a module's source
// beside the manifest, as a directory under the path policy (read when the
// build is admitted) or a git repository (cloned with the host's git and
// credentials when the build starts, its ref resolved to a commit).

// SourceOverride supplies one module's source: Dir, or Git with an
// optional Ref and Subdir.
type SourceOverride struct {
	// Module is the module path the manifest names or replaces.
	Module string
	// Dir is a directory under the path policy holding the module's go.mod.
	Dir string
	// Git is a repository URL or path for the host's git.
	Git string
	// Ref is a branch, tag or commit; empty means the repository's HEAD.
	Ref string
	// Subdir is the module's directory in the repository.
	Subdir string
	// Archive is an uncompressed tar of the module's directory, the only
	// source a team server builds from.
	Archive []byte
}

// BuildSource is a module source a RuntimeStore builds with: a directory's
// snapshot, or a git source whose Files and Commit the build queue fills
// before the store builds.
type BuildSource struct {
	Module string
	// Origin says where the source came from: the directory, or the
	// repository with its ref and subdirectory.
	Origin string
	Files  fs.FS
	// Git is set for a git source.
	Git *SourceOverride
	// Commit is the git source's resolved commit.
	Commit string
}

// Source tree limits: a module source is code, not data.
var sourceLimits = hostfs.TreeLimits{Files: 20000, Bytes: 64 << 20, Skip: []string{".git", ".hg", ".svn", "node_modules"}}

func (s *Service) versionTable() *versions.Table {
	if s.versions != nil {
		return s.versions
	}
	return versions.Embedded()
}

// buildSources checks the overrides and the manifest's local directories
// (`path:` entries and replaces with a directory), relative to base (the
// manifest file's directory or the request's ManifestDir), and reads every
// directory under the path policy.
func (s *Service) buildSources(m *manifest.Manifest, base string, overrides []SourceOverride) ([]BuildSource, error) {
	var problems []string
	var out []BuildSource
	seen := map[string]bool{}
	read := func(module, dir, what string) {
		files, err := s.policy.Paths.ReadTree(dir, sourceLimits)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s for %s: %v", what, module, err))
			return
		}
		if _, ok := files["go.mod"]; !ok {
			problems = append(problems, fmt.Sprintf("%s for %s: %s has no go.mod", what, module, dir))
			return
		}
		out = append(out, BuildSource{Module: module, Origin: dir, Files: mapFS(files)})
	}
	for _, o := range overrides {
		if p := overrideProblem(o, seen); p != "" {
			problems = append(problems, p)
			continue
		}
		seen[o.Module] = true
		if o.Dir != "" {
			read(o.Module, o.Dir, "the source directory")
			continue
		}
		g := o
		out = append(out, BuildSource{Module: o.Module, Origin: gitOrigin(o), Git: &g})
	}
	local := map[string]string{}
	for _, e := range m.Entries() {
		if e.Path != "" {
			local[e.ModulePath()] = e.Path
		}
	}
	replaces, err := m.ParseReplaces()
	if err != nil {
		return nil, manifestDiagnostics("invalid_manifest", err.Error())
	}
	for _, r := range replaces {
		if r.Local() {
			local[r.Old] = r.New
		}
	}
	for module, dir := range local {
		if seen[module] {
			continue // The request's source replaces the manifest's directory.
		}
		seen[module] = true
		if base == "" && !filepath.IsAbs(dir) {
			problems = append(problems, fmt.Sprintf("the manifest's directory %s for %s is relative to a manifest file: supply the source with the build", dir, module))
			continue
		}
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(base, dir)
		}
		read(module, dir, "the manifest's directory")
	}
	if len(problems) > 0 {
		return nil, manifestDiagnostics("manifest_sources", problems...)
	}
	return out, nil
}

// overrideProblem says what is wrong with a source override; seen holds
// the modules already supplied.
func overrideProblem(o SourceOverride, seen map[string]bool) string {
	switch {
	case o.Module == "":
		return "a source override names no module"
	case seen[o.Module]:
		return fmt.Sprintf("%s has more than one source", o.Module)
	case (o.Dir == "") == (o.Git == ""):
		return fmt.Sprintf("the source of %s needs a directory or a git repository, not both", o.Module)
	case o.Dir != "" && (o.Ref != "" || o.Subdir != ""):
		return fmt.Sprintf("the source of %s: a ref and a subdirectory belong to a git repository", o.Module)
	case o.Subdir != "" && (path.IsAbs(o.Subdir) || !fs.ValidPath(path.Clean(filepath.ToSlash(o.Subdir)))):
		return fmt.Sprintf("the source of %s: subdirectory %q must stay inside the repository", o.Module, o.Subdir)
	case strings.HasPrefix(o.Git, "-") || strings.HasPrefix(o.Ref, "-"):
		return fmt.Sprintf("the source of %s: a repository or ref cannot start with '-'", o.Module)
	}
	return ""
}

// customComponents reads each source (a directory under the path policy,
// or a git repository cloned now) and finds its component packages, for a
// manifest that builds them (manifest.Discover).
func (s *Service) customComponents(ctx context.Context, overrides []SourceOverride) ([]catalog.Custom, []string, error) {
	var problems []string
	var out []catalog.Custom
	var providers []string
	seen := map[string]bool{}
	for _, o := range overrides {
		if p := overrideProblem(o, seen); p != "" {
			problems = append(problems, p)
			continue
		}
		seen[o.Module] = true
		var files map[string][]byte
		var err error
		if o.Dir != "" {
			files, err = s.policy.Paths.ReadTree(o.Dir, sourceLimits)
		} else {
			files, _, err = fetchGit(ctx, o, s.policy.Paths.Unrestricted, s.gitClone)
		}
		if err != nil {
			problems = append(problems, fmt.Sprintf("the source of %s (%s): %v", o.Module, cmp.Or(o.Dir, gitOrigin(o)), err))
			continue
		}
		module, found, err := manifest.Discover(mapFS(files))
		switch {
		case err != nil:
			problems = append(problems, fmt.Sprintf("the source of %s: %v", o.Module, err))
		case module != o.Module:
			problems = append(problems, fmt.Sprintf("the source of %s declares module %s in its go.mod", o.Module, module))
		case len(found) == 0:
			problems = append(problems, fmt.Sprintf("the source of %s has no component: no package has a NewFactory() returning a Collector receiver, processor, exporter, connector or extension Factory", o.Module))
		}
		for _, c := range found {
			out = append(out, catalog.Custom{Module: module, Component: c})
		}
		if _, ps, _, err := manifest.DiscoverProviders(mapFS(files)); err == nil {
			for _, p := range ps {
				providers = append(providers, cmp.Or(p.Name, "?")+" "+p.Import)
			}
		}
	}
	if len(problems) > 0 {
		return nil, nil, manifestDiagnostics("manifest_sources", problems...)
	}
	return out, providers, nil
}

func gitOrigin(o SourceOverride) string {
	origin := o.Git
	if o.Ref != "" {
		origin += "@" + o.Ref
	}
	if o.Subdir != "" {
		origin += "#" + o.Subdir
	}
	return origin
}

func mapFS(files map[string][]byte) fs.FS {
	m := fstest.MapFS{}
	for name, data := range files {
		m[name] = &fstest.MapFile{Data: data, Mode: 0o644}
	}
	return m
}

// fetchGitSources clones each git source with the host's git, resolves its
// ref to a commit, checks it out and reads the module's directory.
// The clone is removed afterwards.
func fetchGitSources(ctx context.Context, sources []BuildSource, local bool, limit int64, log func(string)) error {
	for i := range sources {
		src := &sources[i]
		if src.Git == nil || src.Files != nil {
			continue
		}
		log("fetching " + src.Module + " from " + src.Origin)
		files, commit, err := fetchGit(ctx, *src.Git, local, limit)
		if err != nil {
			return fmt.Errorf("the source of %s (%s): %w", src.Module, src.Origin, err)
		}
		src.Files, src.Commit = mapFS(files), commit
		log(src.Module + " is commit " + commit)
	}
	return nil
}

// fetchGit clones o's repository. Only a debugger that may read any host
// file (local) clones a local path or file:// URL; a team server reads no
// host file, so git refuses its file transport there. A limit above zero
// bounds the clone's directory, its history and checkout together: git
// is stopped once the directory grows past it.
func fetchGit(ctx context.Context, o SourceOverride, local bool, limit int64) (map[string][]byte, string, error) {
	dir, err := os.MkdirTemp("", "linea-git-")
	if err != nil {
		return nil, "", err
	}
	defer os.RemoveAll(dir)
	clone := filepath.Join(dir, "repo")
	// The build compiles the commit's bytes: no line-ending conversion.
	file := "never"
	if local {
		file = "always"
	}
	bounded := func(args ...string) (string, error) { return gitBounded(ctx, dir, limit, args...) }
	if _, err := bounded("-c", "protocol.file.allow="+file, "clone", "--quiet", "--no-checkout", "--config", "core.autocrlf=false", "--", o.Git, clone); err != nil {
		if !local && strings.Contains(err.Error(), "transport 'file' not allowed") {
			return nil, "", errors.New("a local repository is not allowed: the server reads no host file, so it clones remote repositories only")
		}
		return nil, "", err
	}
	ref := o.Ref
	if ref == "" {
		ref = "HEAD"
	}
	commit, err := git(ctx, clone, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		// A branch other than the default exists only as a remote branch.
		if commit, err = git(ctx, clone, "rev-parse", "--verify", "--quiet", "origin/"+ref+"^{commit}"); err != nil {
			return nil, "", fmt.Errorf("the repository has no ref %s", ref)
		}
	}
	if _, err := bounded("-C", clone, "-c", "advice.detachedHead=false", "checkout", "--quiet", "--detach", commit); err != nil {
		return nil, "", err
	}
	moduleDir := clone
	if o.Subdir != "" {
		moduleDir = filepath.Join(clone, filepath.FromSlash(path.Clean(filepath.ToSlash(o.Subdir))))
	}
	files, err := hostfs.Policy{Roots: []string{clone}}.ReadTree(moduleDir, sourceLimits)
	if err != nil {
		return nil, "", err
	}
	if _, ok := files["go.mod"]; !ok {
		return nil, "", fmt.Errorf("commit %s has no go.mod in %s", commit, cmp.Or(o.Subdir, "its root"))
	}
	return files, commit, nil
}

// gitBounded runs git as git does, stopping it when the directory watch
// grows past limit bytes; a limit of zero or less does not watch.
func gitBounded(ctx context.Context, watch string, limit int64, args ...string) (string, error) {
	if limit <= 0 {
		return git(ctx, "", args...)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var exceeded atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(gitWatchInterval)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if dirSize(watch) > limit {
					exceeded.Store(true)
					cancel()
					return
				}
			}
		}
	}()
	out, err := git(ctx, "", args...)
	cancel()
	<-done
	if exceeded.Load() || err == nil && dirSize(watch) > limit {
		return "", fmt.Errorf("the repository exceeds the clone limit of %d bytes (limits.git_clone_size on a team server)", limit)
	}
	return out, err
}

// gitWatchInterval is how often gitBounded measures the clone.
var gitWatchInterval = 100 * time.Millisecond

// dirSize sums the sizes of the regular files under dir, skipping what it
// cannot read: git creates and renames files while it runs.
func dirSize(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

// git runs the host's git without prompting for credentials it lacks. A
// cancelled git's helpers (git-remote-https) may hold its output open, so
// waiting for them is bounded.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.WaitDelay = 5 * time.Second
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return "", errors.New("git is not installed on this host")
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		sub := args[0]
		for len(args) > 2 && (sub == "-c" || sub == "-C") {
			args = args[2:]
			sub = args[0]
		}
		return "", fmt.Errorf("git %s: %s", sub, msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}
