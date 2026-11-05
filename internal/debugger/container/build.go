package container

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/mod/modfile"

	"github.com/lineahq/linea/internal/debugger/manifest"
	"github.com/lineahq/linea/internal/debugger/release"
	"github.com/lineahq/linea/internal/debugger/worker"
)

// GoImage is the official Go image builds run in, pinned by digest for this
// debugger release. Its Go version matches go.mod's toolchain line.
const GoImage = "golang:1.27.3-bookworm@sha256:d9b6a1beb9474adc2a1ce842573639b44b2a0dd49b6669f51d3821baf9ec37c9"

// GoVersion is the Go release of GoImage, as `go version` and runtime.Version
// report it. A native builder needs exactly this toolchain: only then is its
// binary the one the Docker build produces.
const GoVersion = "go1.27.3"

// Build defaults: a test build compiled every core and contrib component within 5 GB
// at 4 CPUs only with these settings.
const (
	DefaultBuildMemory = 5 << 30
	DefaultBuildCPUs   = 4
	// buildParallel is go build -p: packages compiled at once.
	buildParallel = 2
	// buildGOGC makes the compiler collect more often, trading time for
	// memory.
	buildGOGC = "25"
)

// heavyPackages compile with one backend worker (-c=1): generated packages
// whose compilation alone exceeded 5 GB in that build.
var heavyPackages = []string{
	"github.com/DataDog/datadog-api-client-go/v2/api/datadogV2",
}

// moduleEnv are the Go module variables passed to a build from the
// debugger's environment: the user's module source.
var moduleEnv = []string{"GOPROXY", "GOPRIVATE", "GONOPROXY", "GONOSUMDB", "GOSUMDB", "GOINSECURE"}

// BuildRequest describes a runtime build.
type BuildRequest struct {
	// Name is the runtime's name, the tag of its image (ValidName).
	Name string
	// Manifest is the OCB manifest, kept unchanged in the image's labels.
	Manifest []byte
	// Source is the worker source (workersrc.Embedded or workersrc.Pack).
	Source fs.FS
	// Set is the manifest release's dependency set; nil builds an
	// unverified runtime, resolving the release through the module source.
	Set *DependencySet
	// Sources are module sources supplied beside the manifest.
	Sources []ModuleSource
	// GoImage overrides the Go image; empty means GoImage.
	GoImage string
	// ModuleProxy, when set, is GOPROXY for this build; otherwise the
	// debugger's GOPROXY and related variables pass through, else Go's
	// defaults apply.
	ModuleProxy string
	// Offline builds without network access (GOPROXY=off): every module
	// must already be in the cache volume, else the build fails naming it.
	Offline bool
	// Memory and CPUs limit the build container; zero means the defaults.
	Memory int64
	CPUs   float64
	// Log receives the build's output, line by line.
	Log func(line string)
}

// CustomModule is a module built from a supplied source, as the image's
// labels and reports record it.
type CustomModule struct {
	Module string `json:"module"`
	Source string `json:"source"`
	Commit string `json:"commit,omitempty"`
}

// Provider is a debugger provider a custom module exports:
// its name, when the source states it, and its package.
type Provider struct {
	Name    string `json:"name,omitempty"`
	Package string `json:"package"`
}

// Runtime is a built runtime image.
type Runtime struct {
	Name    string
	Ref     string
	ImageID string
	Labels  map[string]string
	// Hello is what the image's worker announced when the build started it.
	Hello worker.Hello
}

// BuildError is a build that failed after it started resolving or
// compiling, or while its worker started; Output is the tail of the Go or
// worker output.
type BuildError struct {
	// What is what was built, "runtime" when empty.
	What   string
	Stage  string
	Output string
	Err    error
	// Missing are the modules the Go image could not load, such as those
	// missing from the module cache of an offline build.
	Missing []string
}

func (e *BuildError) Error() string {
	what := e.What
	if what == "" {
		what = "runtime"
	}
	msg := fmt.Sprintf("%s build failed while %s: %v", what, e.Stage, e.Err)
	if len(e.Missing) > 0 {
		msg += "\nmodules that could not be loaded: " + strings.Join(e.Missing, ", ")
	}
	if out := strings.TrimSpace(e.Output); out != "" {
		msg += "\n" + out
	}
	return msg
}

func (e *BuildError) Unwrap() error { return e.Err }

// Build compiles a runtime from a manifest and the worker source, wraps the
// worker in an image FROM scratch, and starts it once to check that it says
// hello; an image whose worker does not start is removed and the build
// fails with the worker's output. A build whose requirements differ from the
// dependency set (custom modules, replaces, excludes, an unverified release)
// resolves them in the Go image first and is refused if that moved a core or
// contrib module off the release.
func (d *Docker) Build(ctx context.Context, req BuildRequest) (*Runtime, error) {
	m, p, err := checkedPlan(req)
	if err != nil {
		return nil, err
	}
	if _, err := d.Version(ctx); err != nil {
		return nil, err
	}
	goImage := req.GoImage
	if goImage == "" {
		goImage = GoImage
	}
	log := req.Log
	if log == nil {
		log = func(string) {}
	}
	if !p.verified {
		log("Collector " + p.collectorVersion + " is not in this debugger's support table: building an unverified runtime")
	}

	archive, goMod, goSum, err := p.buildModule(req, " in "+goImage, log,
		func(archive, goMod []byte) ([]byte, []byte, error) {
			out, err := d.runGo(ctx, req, goImage, "resolving modules", archive, goMod, missingArgs,
				"go "+shellJoin(resolveArgs)+" >/dev/null", "tar -cf - go.mod go.sum", log)
			if err != nil {
				return nil, nil, err
			}
			goMod, goSum, err := untarModFiles(out)
			if err != nil {
				return nil, nil, &BuildError{Stage: "resolving modules", Err: err}
			}
			return goMod, goSum, nil
		})
	if err != nil {
		return nil, err
	}
	log("compiling the worker in " + goImage)
	// The feature gates' owners come from the resolved modules' source, so
	// they are generated here, before the worker compiles.
	bin, err := d.runGo(ctx, req, goImage, "compiling", archive, goMod, missingArgs,
		"{ go "+shellJoin(genArgs)+" 1>&2 && go "+shellJoin(compileArgs("/tmp/worker", p.verified))+" 1>&2; }", "cat /tmp/worker", log)
	if err != nil {
		return nil, err
	}
	if len(bin) == 0 {
		return nil, &BuildError{Stage: "compiling", Err: errors.New("the build wrote no worker")}
	}
	labels, err := runtimeLabels(req, p, goSum, goImage)
	if err != nil {
		return nil, err
	}
	ref := Ref(req.Name)
	log("packaging the runtime image " + ref)
	imageID, err := d.packageImage(ctx, ref, bin, labels)
	if err != nil {
		return nil, &BuildError{Stage: "packaging the image", Err: err}
	}

	log("starting the runtime's worker")
	w, err := d.Start(ctx, imageID, req.Name, RunOptions{})
	if err != nil {
		// The error ends with the worker's log (a panic at start).
		_, _ = d.output(context.Background(), nil, "image", "rm", "-f", ref)
		return nil, &BuildError{Stage: "starting the worker", Err: err}
	}
	hello := w.Hello()
	_ = w.Close()
	if hello.CollectorVersion != p.collectorVersion || !slices.Equal(hello.Modules, m.Modules()) {
		_, _ = d.output(context.Background(), nil, "image", "rm", "-f", ref)
		return nil, &BuildError{Stage: "starting the worker", Err: fmt.Errorf("the worker announced Collector %s with %d modules, the manifest names %s with %d", hello.CollectorVersion, len(hello.Modules), p.collectorVersion, len(m.Modules()))}
	}
	log("built " + ref)
	return &Runtime{Name: req.Name, Ref: ref, ImageID: imageID, Labels: labels, Hello: hello}, nil
}

// runtimeLabels are a runtime image's labels: what the build was made from,
// read back by ReadRuntimeImage. Docker builds and native builds that export
// an image label it alike.
func runtimeLabels(req BuildRequest, p *buildPlan, goSum []byte, goImage string) (map[string]string, error) {
	labels := map[string]string{
		LabelRuntime:          req.Name,
		LabelProductVersion:   release.ProductVersion,
		LabelCollectorVersion: p.collectorVersion,
		LabelProtocol:         strconv.Itoa(worker.ProtocolVersion),
		LabelVerified:         strconv.FormatBool(p.verified),
		LabelManifest:         string(req.Manifest),
		LabelModuleSet:        moduleSet(goSum),
		LabelGoImage:          goImage,
	}
	if len(req.Sources) > 0 {
		custom := make([]CustomModule, 0, len(req.Sources))
		for _, s := range req.Sources {
			custom = append(custom, CustomModule{Module: s.Module, Source: s.Origin, Commit: s.Commit})
		}
		raw, err := json.Marshal(custom)
		if err != nil {
			return nil, err
		}
		labels[LabelCustomModules] = string(raw)
	}
	if found, err := sourceProviders(req.Sources); err != nil {
		return nil, err
	} else if len(found) > 0 {
		provs := make([]Provider, 0, len(found))
		for _, pr := range found {
			provs = append(provs, Provider{Name: pr.Name, Package: pr.Import})
		}
		raw, err := json.Marshal(provs)
		if err != nil {
			return nil, err
		}
		labels[LabelProviders] = string(raw)
	}
	return labels, nil
}

// checkedPlan is what both builders do before any step runs: the name, the
// manifest, the plan against the dependency set and the sources.
func checkedPlan(req BuildRequest) (*manifest.Manifest, *buildPlan, error) {
	if !ValidName(req.Name) {
		return nil, nil, fmt.Errorf("runtime name %q: use lower-case letters, digits, '_', '.' and '-', starting with a letter or digit, at most 63 characters", req.Name)
	}
	return plan(req)
}

// plan parses the manifest and plans the build module against the
// dependency set and the sources; it ignores the request's name.
func plan(req BuildRequest) (*manifest.Manifest, *buildPlan, error) {
	m, err := manifest.Parse(req.Manifest)
	if err != nil {
		return nil, nil, err
	}
	p, err := planBuild(req.Source, m, req.Set, sourced(req.Sources))
	if err != nil {
		return nil, nil, err
	}
	if err := p.checkSources(req.Sources); err != nil {
		return nil, nil, err
	}
	return m, p, nil
}

// resolveFunc runs the resolve step on a build module (a gzipped tar) and
// returns the go.mod and go.sum it resolved.
type resolveFunc func(archive, goMod []byte) (resolvedMod, resolvedSum []byte, err error)

// buildModule is the sequence both builders share: when the plan's
// requirements changed from the dependency set, resolve them with the
// builder's step and refuse a resolution that moved a core or contrib module
// off the release; then write the build module the compile step takes.
// where says where the builder runs its steps, for the log.
func (p *buildPlan) buildModule(req BuildRequest, where string, log func(string), resolve resolveFunc) (archive, goMod, goSum []byte, err error) {
	goMod, goSum = p.goMod, p.goSum
	if p.resolve {
		first, err := p.writeArchive(req.Source, goMod, goSum, req.Sources)
		if err != nil {
			return nil, nil, nil, err
		}
		log("resolving modules" + where)
		if goMod, goSum, err = resolve(first, goMod); err != nil {
			return nil, nil, nil, err
		}
		if err := p.checkResolved(goMod); err != nil {
			return nil, nil, nil, err
		}
	}
	if archive, err = p.writeArchive(req.Source, goMod, goSum, req.Sources); err != nil {
		return nil, nil, nil, err
	}
	return archive, goMod, goSum, nil
}

// The Go commands of a build's steps, one source for the Docker and the
// native builder: a runtime built either way is compiled by the same
// commands, so it is the same binary.
var (
	// resolveArgs loads the worker's packages, which resolves the module
	// requirements into go.mod and go.sum.
	resolveArgs = resolveArgsFor(workerMain)
	// genArgs writes the feature gates' owners from the resolved modules.
	genArgs = []string{"run", "-mod=readonly", "./internal/debugger/registry/gen", "-gates", "internal/debugger/registry/gateowners.go", "-source", "the runtime's manifest"}
	// missingArgs lists, after a failed step, the packages Go could not load.
	missingArgs = missingArgsFor(workerMain)
)

// workerMain is the main package a runtime build compiles; cliMain is the
// one a CLI build compiles (cli.go).
const (
	workerMain = "./cmd/linea-worker"
	cliMain    = "./cmd/linea"
)

// resolveArgsFor loads a main package's packages, which resolves the module
// requirements into go.mod and go.sum.
func resolveArgsFor(main string) []string {
	return []string{"list", "-mod=mod", "-deps", main}
}

// missingArgsFor lists, after a failed step, the packages of a main package
// Go could not load.
func missingArgsFor(main string) []string {
	return []string{"list", "-e", "-deps", "-f", "{{if .Error}}" + missingMarker + "{{.ImportPath}}{{end}}", main}
}

// unverifiedVariable is the worker variable that marks a runtime of a
// Collector release outside the support table, so its hello says so.
const unverifiedVariable = "github.com/lineahq/linea/internal/debugger/worker.unverified"

// customVariable is the worker variable that marks every runtime build, so
// its hello never claims the built-in runtime.
const customVariable = "github.com/lineahq/linea/internal/debugger/worker.custom"

// compileArgs is the go build of the worker into out, for a verified
// release or not.
func compileArgs(out string, verified bool) []string {
	ldflags := "-ldflags=-s -w -X " + release.VersionVariable + "=" + release.ProductVersion + " -X " + customVariable + "=true"
	if !verified {
		ldflags += " -X " + unverifiedVariable + "=true"
	}
	return buildArgs(ldflags, out, workerMain)
}

// buildArgs is the go build of a main package into out with ldflags.
func buildArgs(ldflags, out, main string) []string {
	args := []string{"build", "-mod=readonly", "-p", strconv.Itoa(buildParallel), "-trimpath", "-buildvcs=false", ldflags}
	for _, h := range heavyPackages {
		args = append(args, "-gcflags="+h+"=-c=1")
	}
	return append(args, "-o", out, main)
}

// shellJoin quotes args for a POSIX shell script.
func shellJoin(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = shellQuote(a)
	}
	return strings.Join(quoted, " ")
}

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

func shellQuote(s string) string {
	if shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func sourced(sources []ModuleSource) []string {
	out := make([]string, 0, len(sources))
	for _, s := range sources {
		out = append(out, s.Module)
	}
	return out
}

// missingMarker prefixes the packages the Go image could not load, listed
// after a failed step.
const missingMarker = "linea-missing "

// runGo runs a step in the Go image: the build module arrives as a tar on
// the container's standard input, step runs in it with its output as the
// log, and emit writes the step's result to standard output. When step
// fails, the packages Go could not load are listed for the error with
// missing.
func (d *Docker) runGo(ctx context.Context, req BuildRequest, goImage, stage string, archive, goMod []byte, missing []string, step, emit string, log func(string)) ([]byte, error) {
	memory := req.Memory
	if memory <= 0 {
		memory = DefaultBuildMemory
	}
	cpus := req.CPUs
	if cpus <= 0 {
		cpus = min(DefaultBuildCPUs, float64(runtime.NumCPU()))
	}
	// tar -m stamps the files with the extraction time: the archive's fixed
	// times would let Go's file cache in the shared build cache volume hand
	// a build another build's file of the same path and size.
	script := strings.Join([]string{
		"set -e",
		"mkdir -p /src && cd /src && tar -xzmf -",
		"if ! " + step + "; then go " + shellJoin(missing) + " 1>&2 2>/dev/null || true; exit 1; fi",
		emit,
	}, "\n")
	name := "linea-build-" + randomSuffix()
	args := []string{
		"run", "-i", "--rm", "--name", name,
		"--memory", strconv.FormatInt(memory, 10), "--memory-swap", strconv.FormatInt(memory, 10),
		"--cpus", strconv.FormatFloat(cpus, 'f', -1, 64),
		"-v", ModuleCacheVolume + ":/go/pkg/mod",
		"-v", BuildCacheVolume + ":/root/.cache/go-build",
		"-e", "CGO_ENABLED=0", "-e", "GOOS=linux",
		"-e", "GOTOOLCHAIN=local", "-e", "GOGC=" + buildGOGC,
		"-e", "GOMAXPROCS=" + strconv.Itoa(max(1, int(cpus))),
	}
	if req.Offline {
		args = append(args, "--network", "none", "-e", "GOPROXY=off")
	} else {
		for _, kv := range moduleEnvironment(req.ModuleProxy) {
			args = append(args, "-e", kv)
		}
	}
	args = append(args, goImage, "sh", "-c", script)

	cmd := d.command(context.Background(), args...)
	cmd.Stdin = bytes.NewReader(archive)
	var out bytes.Buffer
	cmd.Stdout = &out
	logs := &lineLog{log: log}
	cmd.Stderr = logs
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		d.removeContainer(name)
		<-done
		return nil, &BuildError{Stage: stage, Output: logs.tail(), Err: context.Cause(ctx)}
	}
	logs.flush()
	if err != nil {
		be := &BuildError{Stage: stage, Output: logs.tail(), Err: err, Missing: missingModules(logs.tail(), goMod)}
		if req.Offline && len(be.Missing) > 0 {
			be.Err = fmt.Errorf("the build is offline (GOPROXY=off) and the module cache lacks modules: %w", err)
		}
		return nil, be
	}
	return out.Bytes(), nil
}

// lookupFailure is Go's error for a module it could not look up.
var lookupFailure = regexp.MustCompile(`go: ([^\s@:]+)@(v[^\s:]+): `)

// missingModules names the modules behind the packages a failed step could
// not load: the longest required module path of each, or the package path
// when no requirement provides it.
func missingModules(output string, goMod []byte) []string {
	var required []string
	if f, err := modfile.ParseLax("go.mod", goMod, nil); err == nil {
		for _, r := range f.Require {
			required = append(required, r.Mod.Path+" "+r.Mod.Version)
		}
	}
	var out []string
	add := func(s string) {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if pkg, ok := strings.CutPrefix(line, missingMarker); ok {
			best, bestMod := pkg, ""
			for _, r := range required {
				mod, _, _ := strings.Cut(r, " ")
				if (pkg == mod || strings.HasPrefix(pkg, mod+"/")) && len(mod) > len(bestMod) {
					best, bestMod = r, mod
				}
			}
			add(best)
		} else if m := lookupFailure.FindStringSubmatch(line); m != nil && strings.Contains(line, "GOPROXY=off") {
			add(m[1] + " " + m[2])
		}
	}
	return out
}

// untarModFiles reads go.mod and go.sum from a resolve step's tar.
func untarModFiles(raw []byte) (goMod, goSum []byte, err error) {
	tr := tar.NewReader(bytes.NewReader(raw))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, nil, err
		}
		switch hdr.Name {
		case "go.mod":
			goMod = data
		case "go.sum":
			goSum = data
		}
	}
	if goMod == nil {
		return nil, nil, errors.New("the resolution wrote no go.mod")
	}
	return goMod, goSum, nil
}

// moduleEnvironment is the module source of a build.
func moduleEnvironment(proxy string) []string {
	var env []string
	for _, k := range moduleEnv {
		if k == "GOPROXY" && proxy != "" {
			env = append(env, "GOPROXY="+proxy)
			continue
		}
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// packageImage builds the runtime image FROM scratch from the worker binary,
// sending the context as a tar on standard input, and returns its ID.
func (d *Docker) packageImage(ctx context.Context, ref string, bin []byte, labels map[string]string) (string, error) {
	dockerfile := strings.Join([]string{
		"FROM scratch",
		"COPY worker /worker",
		"USER 65534:65534",
		`ENTRYPOINT ["/worker"]`,
		"",
	}, "\n")
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range []struct {
		name string
		mode int64
		data []byte
	}{{"Dockerfile", 0o644, []byte(dockerfile)}, {"worker", 0o755, bin}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.data)), ModTime: time.Unix(0, 0).UTC()}); err != nil {
			return "", err
		}
		if _, err := tw.Write(f.data); err != nil {
			return "", err
		}
	}
	if err := tw.Close(); err != nil {
		return "", err
	}
	args := []string{"build", "-q", "-t", ref}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		args = append(args, "--label", k+"="+labels[k])
	}
	args = append(args, "-")
	return d.output(ctx, buf.Bytes(), args...)
}

// buildLogLimit bounds the build output kept for an error.
const buildLogLimit = 16 << 10

// lineLog splits output into lines for the build log and keeps its tail.
type lineLog struct {
	log  func(string)
	mu   sync.Mutex
	part []byte
	last []byte
}

func (l *lineLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.last = append(l.last, p...)
	if over := len(l.last) - buildLogLimit; over > 0 {
		l.last = l.last[over:]
	}
	l.part = append(l.part, p...)
	for {
		i := bytes.IndexByte(l.part, '\n')
		if i < 0 {
			break
		}
		l.log(strings.TrimRight(string(l.part[:i]), "\r"))
		l.part = l.part[i+1:]
	}
	return len(p), nil
}

func (l *lineLog) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.part) > 0 {
		l.log(string(l.part))
		l.part = nil
	}
}

func (l *lineLog) tail() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return string(l.last)
}

var _ io.Writer = (*lineLog)(nil)
