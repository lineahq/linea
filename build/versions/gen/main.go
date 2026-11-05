// Command gen writes a release's files in build/versions/<version>/ from
// the release manifest beside them (manifest.yaml, upstream's otelcol-contrib
// manifest, unchanged) and unsupported.yaml. scripts/dependency-set.ps1 runs
// it in the pinned Go image:
//
//	gen manifest -dir build/versions/0.162.0
//
// prints the manifest the dependency set is resolved from: the release
// manifest without its excluded components and replaces.
//
//	gen catalog -dir build/versions/0.162.0
//
// downloads the component modules through the module source, reads each
// component's metadata.yaml and writes catalog.json, after checking that
// dependencies.mod holds every module at the release's version.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
	"golang.org/x/mod/modfile"

	"github.com/lineahq/linea/build/versions"
	"github.com/lineahq/linea/internal/debugger/manifest"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: gen manifest|catalog -dir build/versions/<version>")
		os.Exit(2)
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	dir := fs.String("dir", "", "the release directory")
	_ = fs.Parse(os.Args[2:])
	var err error
	switch os.Args[1] {
	case "manifest":
		err = writeSetManifest(*dir, os.Stdout)
	case "catalog":
		err = writeCatalog(*dir)
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
}

// unsupported is build/versions/<version>/unsupported.yaml.
type unsupported struct {
	Excluded    map[string]string `yaml:"excluded"`
	Unsupported map[string]string `yaml:"unsupported"`
	Replaces    map[string]string `yaml:"replaces"`
	// Types names the component type of a module without metadata.yaml.
	Types map[string]string `yaml:"types"`
}

type release struct {
	dir      string
	manifest *manifest.Manifest
	skip     unsupported
}

func load(dir string) (*release, error) {
	if dir == "" {
		return nil, errors.New("-dir is required")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.yaml"))
	if err != nil {
		return nil, err
	}
	m, err := manifest.Parse(raw)
	if err != nil {
		return nil, err
	}
	r := &release{dir: dir, manifest: m}
	if raw, err = os.ReadFile(filepath.Join(dir, "unsupported.yaml")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := yaml.Unmarshal(raw, &r.skip); err != nil {
		return nil, fmt.Errorf("unsupported.yaml: %w", err)
	}
	if v := filepath.Base(dir); v != m.CollectorVersion() {
		return nil, fmt.Errorf("%s holds the manifest of %s", dir, m.CollectorVersion())
	}
	return r, nil
}

// setManifest is the release manifest without its excluded components and
// replaces.
func (r *release) setManifest() *manifest.Manifest {
	m := *r.manifest
	keep := func(list []manifest.Module) []manifest.Module {
		return slices.DeleteFunc(slices.Clone(list), func(c manifest.Module) bool { _, ok := r.skip.Excluded[c.ModulePath()]; return ok })
	}
	m.Receivers, m.Processors, m.Exporters = keep(m.Receivers), keep(m.Processors), keep(m.Exporters)
	m.Connectors, m.Extensions = keep(m.Connectors), keep(m.Extensions)
	m.Replaces = slices.DeleteFunc(slices.Clone(m.Replaces), func(s string) bool {
		old, _, _ := strings.Cut(s, "=>")
		_, ok := r.skip.Replaces[strings.Fields(old)[0]]
		return ok
	})
	return &m
}

func writeSetManifest(dir string, w io.Writer) error {
	r, err := load(dir)
	if err != nil {
		return err
	}
	m := r.setManifest()
	m.Dist = manifest.Dist{Name: "dependency-set", OtelcolVersion: r.manifest.CollectorVersion()}
	return yaml.NewEncoder(w).Encode(m)
}

// metadata is the part of a component's metadata.yaml the catalog reads.
type metadata struct {
	Type           string `yaml:"type"`
	DeprecatedType string `yaml:"deprecated_type"`
	Status         struct {
		Class     string              `yaml:"class"`
		Stability map[string][]string `yaml:"stability"`
	} `yaml:"status"`
}

func writeCatalog(dir string) error {
	r, err := load(dir)
	if err != nil {
		return err
	}
	set := r.setManifest()
	if err := checkSet(dir, set); err != nil {
		return err
	}
	var args []string
	for _, kind := range manifest.Kinds {
		for _, c := range r.manifest.Components(kind) {
			args = append(args, c.ModulePath()+"@"+c.Version())
		}
	}
	dirs, err := download(args)
	if err != nil {
		return err
	}
	out := versions.Catalog{
		CollectorVersion: r.manifest.CollectorVersion(),
		Source:           "opentelemetry-collector-releases " + r.manifest.Dist.Name + " " + r.manifest.Dist.Version,
		Components:       []versions.Component{},
		Providers:        gomods(r.manifest.Providers),
		Converters:       gomods(r.manifest.Converters),
		Replaces:         append([]string{}, set.Replaces...),
	}
	for _, kind := range manifest.Kinds {
		for _, c := range r.manifest.Components(kind) {
			entry := versions.Component{Kind: string(kind), GoMod: c.ModulePath() + " " + c.Version(), Import: c.Import, Stability: map[string][]string{}}
			entry.Class = "contrib"
			if strings.HasPrefix(c.ModulePath(), "go.opentelemetry.io/collector/") {
				entry.Class = "core"
			}
			entry.Unsupported = r.skip.Excluded[c.ModulePath()] + r.skip.Unsupported[c.ModulePath()]
			if t, ok := r.skip.Types[c.ModulePath()]; ok {
				entry.Type = t
			} else {
				md, err := readMetadata(dirs[c.ModulePath()], strings.TrimPrefix(strings.TrimPrefix(c.ImportPath(), c.ModulePath()), "/"))
				if err != nil {
					return fmt.Errorf("%s: %w", c.ModulePath(), err)
				}
				entry.Type = md.Type
				entry.DeprecatedType = md.DeprecatedType
				for level, signals := range md.Status.Stability {
					entry.Stability[level] = signals
				}
			}
			out.Components = append(out.Components, entry)
		}
	}
	slices.SortStableFunc(out.Components, func(a, b versions.Component) int {
		return cmpKinds(a.Kind, b.Kind)*1000 + strings.Compare(a.Type, b.Type)
	})
	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "catalog.json"), append(raw, '\n'), 0o644)
}

func cmpKinds(a, b string) int {
	ia := slices.Index(manifest.Kinds, manifest.Kind(a))
	ib := slices.Index(manifest.Kinds, manifest.Kind(b))
	switch {
	case ia < ib:
		return -1
	case ia > ib:
		return 1
	}
	return 0
}

func gomods(list []manifest.Module) []string {
	out := []string{}
	for _, c := range list {
		out = append(out, c.ModulePath()+" "+c.Version())
	}
	return out
}

// checkSet fails unless dependencies.mod requires every module of the set
// manifest at its version.
func checkSet(dir string, set *manifest.Manifest) error {
	raw, err := os.ReadFile(filepath.Join(dir, "dependencies.mod"))
	if err != nil {
		return err
	}
	f, err := modfile.ParseLax("dependencies.mod", raw, nil)
	if err != nil {
		return err
	}
	have := map[string]string{}
	for _, r := range f.Require {
		have[r.Mod.Path] = r.Mod.Version
	}
	var problems []string
	for _, kind := range manifest.Kinds {
		for _, c := range set.Components(kind) {
			if v := have[c.ModulePath()]; v != c.Version() {
				problems = append(problems, fmt.Sprintf("%s %s, the set has %q", c.ModulePath(), c.Version(), v))
			}
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("dependencies.mod does not hold the release: %s", strings.Join(problems, "; "))
	}
	return nil
}

// download fetches modules through the module source and returns their
// directories in the module cache.
func download(args []string) (map[string]string, error) {
	cmd := exec.Command("go", append([]string{"mod", "download", "-json"}, args...)...)
	cmd.Stderr = os.Stderr
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	runErr := cmd.Run()
	out := map[string]string{}
	dec := json.NewDecoder(&stdout)
	var failed []string
	for {
		var m struct{ Path, Dir, Error string }
		if err := dec.Decode(&m); err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		if m.Error != "" {
			failed = append(failed, m.Path+": "+m.Error)
			continue
		}
		out[m.Path] = m.Dir
	}
	if len(failed) > 0 || (runErr != nil && len(out) == 0) {
		return nil, fmt.Errorf("go mod download: %v %s", runErr, strings.Join(failed, "; "))
	}
	return out, nil
}

func readMetadata(dir, sub string) (*metadata, error) {
	if dir == "" {
		return nil, errors.New("not downloaded")
	}
	raw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(sub), "metadata.yaml"))
	if err != nil {
		return nil, err
	}
	var md metadata
	if err := yaml.Unmarshal(raw, &md); err != nil {
		return nil, fmt.Errorf("metadata.yaml: %w", err)
	}
	if md.Type == "" {
		return nil, errors.New("metadata.yaml names no type")
	}
	return &md, nil
}
