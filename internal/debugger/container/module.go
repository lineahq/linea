package container

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/semver"

	"github.com/lineahq/linea/collector/provider"
	"github.com/lineahq/linea/internal/debugger/features"
	"github.com/lineahq/linea/internal/debugger/manifest"
)

// The generated files the build module replaces with the manifest's: the
// registry, the OTTL variant bindings for its release and its optional
// features.
const (
	registryFile   = "internal/debugger/registry/components.go"
	variantFile    = "internal/debugger/compat/ottl/variant.go"
	featureSetFile = "internal/debugger/featureset/featureset.go"
	providersFile  = "internal/debugger/registry/providers.go"
	releaseFile    = "internal/debugger/release/collector.go"
)

// sdkDir is where the worker source holds the provider SDK module;
// every build replaces the SDK with it, so the version a custom
// module requires is never downloaded.
const sdkDir = "collector/provider"

// placeholderVersion is the version a sourced module without a manifest
// entry (the target of a local replace) is required at.
const placeholderVersion = "v0.0.0-00010101000000-000000000000"

// ModuleError lists what keeps a manifest from building: each problem names
// the module and, for a version conflict, both versions.
type ModuleError struct {
	Problems []string
}

func (e *ModuleError) Error() string {
	return "the manifest cannot build: " + strings.Join(e.Problems, "; ")
}

// DependencySet is a verified release's go.mod and go.sum: every
// build for the release uses the whole set's requirements.
type DependencySet struct {
	GoMod, GoSum []byte
}

// ModuleSource is a module's source supplied beside the manifest: a
// local directory or a git checkout, copied into the build and replacing
// the module.
type ModuleSource struct {
	// Module is the module path it replaces: a manifest entry's, or a local
	// replace's.
	Module string
	// Files is the module's directory, with its go.mod at the root. It may
	// be nil for Check.
	Files fs.FS
	// Origin says where it came from: a directory, or a git URL with its
	// ref and subdirectory.
	Origin string
	// Commit is the resolved git commit; empty for a directory.
	Commit string
}

// buildPlan is a checked build: the build module's requirements before
// resolution and what resolution must keep.
type buildPlan struct {
	m                *manifest.Manifest
	collectorVersion string
	verified         bool
	goMod, goSum     []byte
	// resolve means the requirements changed from the set and must be
	// resolved in the Go image before compiling (custom modules, replaces,
	// excludes, or an unverified release).
	resolve bool
	// keep are the core and contrib versions resolution must not move.
	keep map[string]string
	// sourceDirs maps a sourced module to its directory in the build.
	sourceDirs map[string]string
	// extra are files the build module gains beside the worker source's,
	// by slash-separated path: a CLI build's stamp (cli.go).
	extra map[string][]byte
}

// planBuild checks a manifest against a dependency set, or against the
// worker source's own requirements for an unverified release (set nil), and
// writes the build module's go.mod. sourced lists the modules with a
// ModuleSource. The debugger is never a required module: its requirements
// would raise the user's Collector version.
func planBuild(src fs.FS, m *manifest.Manifest, set *DependencySet, sourced []string) (*buildPlan, error) {
	srcMod, err := fs.ReadFile(src, "go.mod")
	if err != nil {
		return nil, fmt.Errorf("worker source: %w", err)
	}
	p := &buildPlan{m: m, collectorVersion: m.CollectorVersion(), verified: set != nil, keep: map[string]string{}, sourceDirs: map[string]string{}}
	release := "v" + p.collectorVersion
	var problems []string

	base := srcMod
	if set != nil {
		base, p.goSum = set.GoMod, set.GoSum
	} else if p.goSum, err = fs.ReadFile(src, "go.sum"); err != nil {
		return nil, fmt.Errorf("worker source: %w", err)
	}
	f, err := modfile.Parse("go.mod", base, nil)
	if err != nil {
		return nil, fmt.Errorf("dependency set: %w", err)
	}
	if sf, err := modfile.ParseLax("go.mod", srcMod, nil); err == nil && sf.Module != nil {
		_ = f.AddModuleStmt(sf.Module.Mod.Path)
	}
	required := map[string]string{}
	for _, r := range f.Require {
		required[r.Mod.Path] = r.Mod.Version
	}

	if set != nil {
		if have := required["go.opentelemetry.io/collector/otelcol"]; have != release {
			problems = append(problems, fmt.Sprintf("dist.otelcol_version is %s, the dependency set is %s", p.collectorVersion, strings.TrimPrefix(have, "v")))
		}
		for mod, v := range required {
			if manifest.Upstream(mod) {
				p.keep[mod] = v
			}
		}
	} else {
		// An unverified release: the worker source's core and contrib
		// requirements move to the release, v1 core modules to the stable
		// line the manifest names; resolution finds the rest.
		stable := ""
		for _, e := range m.Entries() {
			if mod := e.ModulePath(); manifest.Upstream(mod) && !strings.HasPrefix(mod, "github.com/") && semver.Major(e.Version()) == "v1" {
				if stable == "" || semver.Compare(e.Version(), stable) > 0 {
					stable = e.Version()
				}
			}
		}
		for _, r := range f.Require {
			switch {
			case !manifest.Upstream(r.Mod.Path):
			case semver.Major(r.Mod.Version) == "v0":
				_ = f.AddRequire(r.Mod.Path, release)
			case stable != "" && !strings.HasPrefix(r.Mod.Path, "github.com/"):
				_ = f.AddRequire(r.Mod.Path, stable)
			}
		}
		p.resolve = true
	}

	sourceDir := func(mod string) string {
		if dir, ok := p.sourceDirs[mod]; ok {
			return dir
		}
		dir := "custom/" + strconv.Itoa(len(p.sourceDirs))
		p.sourceDirs[mod] = dir
		return dir
	}
	entries := map[string]bool{}
	for _, e := range m.Entries() {
		if strings.HasPrefix(e.Where, "providers") || strings.HasPrefix(e.Where, "converters") {
			continue // The debugger resolves configurations with its own providers.
		}
		mod, version := e.ModulePath(), e.Version()
		entries[mod] = true
		have, inSet := required[mod]
		switch {
		case slices.Contains(sourced, mod):
			_ = f.AddRequire(mod, version)
			_ = f.AddReplace(mod, "", "./"+sourceDir(mod), "")
			p.resolve = true
		case set != nil && inSet && (have == version || !manifest.Upstream(mod) && semver.Compare(version, have) < 0):
			// In the set, or a third-party module the set already has at a
			// later version, which the build takes as the set does.
		case set != nil && inSet && manifest.Upstream(mod):
			problems = append(problems, fmt.Sprintf("%s: %s %s, the dependency set has %s", e.Where, mod, version, have))
		default:
			_ = f.AddRequire(mod, version)
			p.resolve = true
		}
	}
	replaces, err := m.ParseReplaces()
	if err != nil {
		return nil, err
	}
	for _, r := range replaces {
		if r.Local() {
			if !slices.Contains(sourced, r.Old) {
				problems = append(problems, fmt.Sprintf("replaces: %s => %s: the directory was not supplied", r.Old, r.New))
				continue
			}
			if !entries[r.Old] {
				if _, ok := required[r.Old]; !ok {
					_ = f.AddRequire(r.Old, placeholderVersion)
				}
				_ = f.AddReplace(r.Old, r.OldVersion, "./"+sourceDir(r.Old), "")
				p.resolve = true
			}
			continue
		}
		if slices.ContainsFunc(f.Replace, func(have *modfile.Replace) bool {
			return have.Old.Path == r.Old && have.Old.Version == r.OldVersion && have.New.Path == r.New && have.New.Version == r.NewVersion
		}) {
			continue
		}
		_ = f.AddReplace(r.Old, r.OldVersion, r.New, r.NewVersion)
		p.resolve = true
	}
	for _, mod := range sourced {
		if _, ok := p.sourceDirs[mod]; !ok {
			problems = append(problems, fmt.Sprintf("a source is supplied for %s, which the manifest neither names nor replaces with a directory: write the manifest with the source to add its components, or add an entry with gomod: %s %s", mod, mod, manifest.SourceVersion))
		}
	}
	_ = f.AddRequire(manifest.ProviderSDK, "v"+provider.Version)
	_ = f.AddReplace(manifest.ProviderSDK, "", "./"+sdkDir, "")
	for _, x := range m.Excludes {
		mod, version, ok := strings.Cut(strings.TrimSpace(x), " ")
		if !ok {
			problems = append(problems, fmt.Sprintf("excludes: %q must be \"<module> <version>\"", x))
			continue
		}
		_ = f.AddExclude(mod, strings.TrimSpace(version))
		p.resolve = true
	}
	if len(problems) > 0 {
		return nil, &ModuleError{Problems: problems}
	}
	f.Cleanup()
	if p.goMod, err = f.Format(); err != nil {
		return nil, err
	}
	return p, nil
}

// checkSources refuses, before resolving, a custom module whose go.mod
// requires a core or contrib module above the verified release's version:
// resolution would move it off the release, often failing first on a
// package a later release removed. Unverified releases resolve freely.
func (p *buildPlan) checkSources(sources []ModuleSource) error {
	if err := checkSDK(sources); err != nil {
		return err
	}
	if !p.verified {
		return nil
	}
	const shown = 5
	var problems []string
	more := 0
	for _, s := range sources {
		if s.Files == nil {
			continue
		}
		data, err := fs.ReadFile(s.Files, "go.mod")
		if err != nil {
			continue
		}
		f, err := modfile.ParseLax("go.mod", data, nil)
		if err != nil {
			continue
		}
		for _, r := range f.Require {
			have, kept := p.keep[r.Mod.Path]
			if !kept || semver.Compare(r.Mod.Version, have) <= 0 {
				continue
			}
			if len(problems) == shown {
				more++
				continue
			}
			problems = append(problems, fmt.Sprintf("%s requires %s %s; Collector %s has %s", s.Module, r.Mod.Path, r.Mod.Version, p.collectorVersion, have))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	if more > 0 {
		problems = append(problems, fmt.Sprintf("and %d more", more))
	}
	return &ModuleError{Problems: append([]string{"a custom module needs a newer Collector than " + p.collectorVersion +
		": build at the release its go.mod requires, or lower its requirements to " + p.collectorVersion}, problems...)}
}

// checkSDK refuses a custom module that requires a later provider SDK than
// the debugger embeds, naming both versions.
func checkSDK(sources []ModuleSource) error {
	var problems []string
	for _, s := range sources {
		if s.Files == nil {
			continue
		}
		data, err := fs.ReadFile(s.Files, "go.mod")
		if err != nil {
			continue
		}
		f, err := modfile.ParseLax("go.mod", data, nil)
		if err != nil {
			continue
		}
		for _, r := range f.Require {
			if r.Mod.Path == manifest.ProviderSDK && semver.Compare(r.Mod.Version, "v"+provider.Version) > 0 {
				problems = append(problems, fmt.Sprintf("%s requires the provider SDK %s; this debugger embeds v%s: update the debugger, or require v%s", s.Module, r.Mod.Version, provider.Version, provider.Version))
			}
		}
	}
	if len(problems) > 0 {
		return &ModuleError{Problems: problems}
	}
	return nil
}

// sourceProviders lists the debugger providers the module sources export,
// in import path order.
func sourceProviders(sources []ModuleSource) ([]manifest.Provider, error) {
	var out []manifest.Provider
	for _, s := range sources {
		if s.Files == nil {
			continue
		}
		_, found, _, err := manifest.DiscoverProviders(s.Files)
		if err != nil {
			return nil, fmt.Errorf("source of %s: %w", s.Module, err)
		}
		out = append(out, found...)
	}
	slices.SortFunc(out, func(a, b manifest.Provider) int { return strings.Compare(a.Import, b.Import) })
	return out, nil
}

// checkResolved refuses a resolution that moved a core or contrib module
// off the release, naming the module and both versions.
func (p *buildPlan) checkResolved(goMod []byte) error {
	f, err := modfile.ParseLax("go.mod", goMod, nil)
	if err != nil {
		return err
	}
	release := "v" + p.collectorVersion
	var problems []string
	for _, r := range f.Require {
		mod, got := r.Mod.Path, r.Mod.Version
		if !manifest.Upstream(mod) {
			continue
		}
		want, kept := p.keep[mod]
		switch {
		case kept && got != want:
			problems = append(problems, fmt.Sprintf("%s moved from %s to %s", mod, want, got))
		case !kept && semver.Major(got) == "v0" && got != release:
			problems = append(problems, fmt.Sprintf("%s is at %s, the release is %s", mod, got, release))
		}
	}
	if len(problems) > 0 {
		slices.Sort(problems)
		return &ModuleError{Problems: append([]string{"a custom module moved a Collector module off the " + p.collectorVersion + " release"}, problems...)}
	}
	return nil
}

// moduleSet is "sha256:<hex>" over a go.sum.
func moduleSet(goSum []byte) string {
	sum := sha256.Sum256(goSum)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// writeArchive writes the build module as a gzipped tar: the worker source
// with the manifest's registry and release line, the plan's go.mod and goSum, and each
// module source in its directory.
func (p *buildPlan) writeArchive(src fs.FS, goMod, goSum []byte, sources []ModuleSource) ([]byte, error) {
	registry, err := manifest.GenerateRegistry(p.m, manifest.RegistryOptions{
		Package: "registry", Source: "the runtime's manifest", Overrides: manifest.RegistryOverrides,
	})
	if err != nil {
		return nil, err
	}
	variant, err := manifest.GenerateOTTLVariant(p.collectorVersion, "the runtime's manifest")
	if err != nil {
		return nil, err
	}
	featureSet, err := manifest.GenerateFeatureSet(p.m, features.Packages(), "the runtime's manifest")
	if err != nil {
		return nil, err
	}
	rel, err := manifest.GenerateRelease(p.m, "the runtime's manifest")
	if err != nil {
		return nil, err
	}
	found, err := sourceProviders(sources)
	if err != nil {
		return nil, err
	}
	imports := make([]manifest.ProviderImport, 0, len(found))
	for _, pr := range found {
		imports = append(imports, manifest.ProviderImport{Package: pr.Import})
	}
	providers, err := manifest.GenerateProviders(imports, "the runtime's sources")
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	add := func(name string, data []byte) error {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), ModTime: time.Unix(0, 0).UTC(), Format: tar.FormatPAX}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	replaced := map[string][]byte{registryFile: registry, variantFile: variant, featureSetFile: featureSet, providersFile: providers, releaseFile: rel, "go.mod": goMod, "go.sum": goSum}
	names, err := files(src)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		data, ok := replaced[name]
		if !ok {
			if data, err = fs.ReadFile(src, name); err != nil {
				return nil, err
			}
		}
		delete(replaced, name)
		if err := add(name, data); err != nil {
			return nil, err
		}
	}
	for _, name := range []string{registryFile, variantFile, featureSetFile, providersFile, releaseFile, "go.mod", "go.sum"} {
		if data, ok := replaced[name]; ok {
			if err := add(name, data); err != nil {
				return nil, err
			}
		}
	}
	extra := make([]string, 0, len(p.extra))
	for name := range p.extra {
		extra = append(extra, name)
	}
	slices.Sort(extra)
	for _, name := range extra {
		if slices.Contains(names, name) {
			return nil, fmt.Errorf("the worker source already has %s", name)
		}
		if err := add(name, p.extra[name]); err != nil {
			return nil, err
		}
	}
	for _, s := range sources {
		dir, ok := p.sourceDirs[s.Module]
		if !ok || s.Files == nil {
			return nil, fmt.Errorf("no source for %s", s.Module)
		}
		names, err := files(s.Files)
		if err != nil {
			return nil, fmt.Errorf("source of %s: %w", s.Module, err)
		}
		if !slices.Contains(names, "go.mod") {
			return nil, fmt.Errorf("source of %s (%s) has no go.mod at its root", s.Module, s.Origin)
		}
		for _, name := range names {
			data, err := fs.ReadFile(s.Files, name)
			if err != nil {
				return nil, err
			}
			if err := add(path.Join(dir, name), data); err != nil {
				return nil, err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// files lists a file system's regular files in name order, skipping version
// control directories.
func files(fsys fs.FS) ([]string, error) {
	var names []string
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == ".hg" || d.Name() == ".svn") {
			return fs.SkipDir
		}
		if d.Type().IsRegular() {
			names = append(names, p)
		}
		return nil
	})
	slices.Sort(names)
	return names, err
}
