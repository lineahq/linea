package container

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lineahq/linea/internal/debugger/worker"
)

// Native builds runtimes with this host's Go toolchain, as linea-builder does:
// the steps of Docker.Build, run as host processes instead of in the Go image.
// Given the pinned toolchain (GoVersion) it produces the binary Docker.Build
// produces from the same manifest and sources: the Go commands are shared
// (resolveArgs, genArgs, compileArgs), and the build environment is the
// container's, CGO_ENABLED=0 GOOS=linux GOTOOLCHAIN=local and the module
// source, with nothing else of the host's passed on.
type Native struct {
	// WorkDir holds everything a build writes: its module and built binary,
	// and the module and build caches unless set below. The directory must
	// allow executing files, because go run starts the gate generator from
	// it. Required.
	WorkDir string
	// CPUs limits the go command's parallelism (GOMAXPROCS); zero means
	// the default build CPUs or the host's, whichever is fewer.
	CPUs int
	// GoCache and ModCache are the build and module caches, which persist
	// across builds; they default to directories under WorkDir.
	GoCache, ModCache string
	// Go is the go executable; empty means "go" on PATH.
	Go string

	// Seams for tests.
	command func(ctx context.Context, args ...string) *exec.Cmd
	hello   func(ctx context.Context, name, binary string) (worker.Hello, error)
}

// NativeRuntime is a runtime built natively: a binary file.
type NativeRuntime struct {
	Name string
	// Path is the worker binary. It belongs to the caller, who removes it
	// with Close.
	Path string
	// Digest is "sha256:<hex>" of the binary and Size its length.
	Digest string
	Size   int64
	// Hello is what the binary announced when the build started it once.
	Hello worker.Hello
	// Labels are the labels a Docker build of the same request gives its
	// image, for an image exported from the binary (PushRuntimeImage).
	Labels map[string]string
}

// Close removes the binary.
func (r *NativeRuntime) Close() error {
	if err := os.Remove(r.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// nativeDirs are the directories under a native builder's work directory.
type nativeDirs struct {
	builds, bin, tmp, modCache, goCache, gopath string
}

func (n *Native) dirs() (nativeDirs, error) {
	if n.WorkDir == "" {
		return nativeDirs{}, errors.New("a native build needs a work directory")
	}
	root, err := filepath.Abs(n.WorkDir)
	if err != nil {
		return nativeDirs{}, err
	}
	d := nativeDirs{
		builds: filepath.Join(root, "builds"), bin: filepath.Join(root, "bin"), tmp: filepath.Join(root, "tmp"),
		modCache: n.ModCache, goCache: n.GoCache, gopath: filepath.Join(root, "gopath"),
	}
	if d.modCache == "" {
		d.modCache = filepath.Join(root, "gomod")
	}
	if d.goCache == "" {
		d.goCache = filepath.Join(root, "gocache")
	}
	for _, dir := range []string{d.builds, d.bin, d.tmp, d.modCache, d.goCache, d.gopath} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nativeDirs{}, err
		}
	}
	return d, nil
}

func (n *Native) cpus() int {
	if n.CPUs > 0 {
		return n.CPUs
	}
	return max(1, min(DefaultBuildCPUs, runtime.NumCPU()))
}

// env is the go command's environment for goos and goarch: the container's
// (Docker.runGo) and the module source of the process's own; GOENV=off and
// GOWORK=off keep the host's Go configuration and workspaces out of it, as
// the image has none. Runtime builds target linux on the host's
// architecture.
func (n *Native) env(d nativeDirs, proxy string, offline bool, goos, goarch string) []string {
	home := os.Getenv("HOME")
	if home == "" {
		home = d.tmp
	}
	env := []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + home,
		"CGO_ENABLED=0", "GOOS=" + goos, "GOARCH=" + goarch,
		"GOTOOLCHAIN=local", "GOGC=" + buildGOGC, "GOMAXPROCS=" + strconv.Itoa(n.cpus()),
		"GOENV=off", "GOWORK=off",
		"GOMODCACHE=" + d.modCache, "GOCACHE=" + d.goCache, "GOPATH=" + d.gopath,
		"GOTMPDIR=" + d.tmp, "TMPDIR=" + d.tmp,
	}
	if offline {
		return append(env, "GOPROXY=off")
	}
	env = append(env, moduleEnvironment(proxy)...)
	for _, k := range moduleAccessEnv {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// moduleAccessEnv are the variables that let the go command reach a private
// module source from the builder's own environment: credentials (NETRC, or
// GOAUTH) and the certificate authorities it trusts (SSL_CERT_FILE,
// SSL_CERT_DIR). They name files in the builder, so they pass to native
// builds only, never into a Docker build's container.
var moduleAccessEnv = []string{"NETRC", "GOAUTH", "SSL_CERT_FILE", "SSL_CERT_DIR"}

func (n *Native) goCommand(ctx context.Context, args ...string) *exec.Cmd {
	if n.command != nil {
		return n.command(ctx, args...)
	}
	path := n.Go
	if path == "" {
		path = "go"
	}
	return exec.CommandContext(ctx, path, args...)
}

// Toolchain returns the Go release of the host's go command, "go1.27.3".
func (n *Native) Toolchain(ctx context.Context) (string, error) {
	d, err := n.dirs()
	if err != nil {
		return "", err
	}
	cmd := n.goCommand(ctx, "env", "GOVERSION")
	cmd.Dir, cmd.Env = d.tmp, n.env(d, "", true, "linux", runtime.GOARCH)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("go env GOVERSION: %v: %s", err, msg)
		}
		return "", fmt.Errorf("go env GOVERSION: %w", err)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// Build compiles a runtime from a manifest and the worker source: it
// extracts the build module into a fresh directory, resolves and compiles it
// with the host's go command, and starts the binary once to check that it
// says hello and announces the manifest's Collector and modules. The build
// directory is removed whatever happens; the binary is left to the caller. A
// failure after the steps started is a BuildError, as in Docker.Build.
// BuildRequest's GoImage and Memory do not apply; the CPUs are Native.CPUs.
func (n *Native) Build(ctx context.Context, req BuildRequest) (*NativeRuntime, error) {
	m, p, err := checkedPlan(req)
	if err != nil {
		return nil, err
	}
	d, err := n.dirs()
	if err != nil {
		return nil, err
	}
	log := req.Log
	if log == nil {
		log = func(string) {}
	}
	if !p.verified {
		log("Collector " + p.collectorVersion + " is not in this debugger's support table: building an unverified runtime")
	}
	buildDir, err := os.MkdirTemp(d.builds, "build-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(buildDir)
	r := &nativeRun{n: n, d: d, req: req, log: log, dir: buildDir, main: workerMain, goos: "linux", goarch: runtime.GOARCH}

	archive, goMod, goSum, err := p.buildModule(req, "", log, func(archive, goMod []byte) ([]byte, []byte, error) {
		return r.resolve(ctx, archive, goMod)
	})
	if err != nil {
		return nil, err
	}
	out, err := os.CreateTemp(d.bin, "worker-*")
	if err != nil {
		return nil, err
	}
	binary := out.Name()
	_ = out.Close()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(binary)
		}
	}()
	log("compiling the worker with " + GoVersion)
	if err := r.compile(ctx, archive, goMod, binary, p.verified); err != nil {
		return nil, err
	}
	digest, size, err := hashFile(binary)
	if err != nil {
		return nil, err
	}
	if size == 0 {
		return nil, &BuildError{Stage: "compiling", Err: errors.New("the build wrote no worker")}
	}
	if err := os.Chmod(binary, 0o755); err != nil {
		return nil, err
	}

	log("starting the runtime's worker")
	start := n.hello
	if start == nil {
		start = func(ctx context.Context, name, binary string) (worker.Hello, error) {
			cmd := exec.Command(binary)
			cmd.Dir, cmd.Env = d.tmp, []string{"TMPDIR=" + d.tmp}
			w, err := worker.Start(ctx, name, cmd)
			if err != nil {
				return worker.Hello{}, err
			}
			hello := w.Hello()
			_ = w.Close()
			return hello, nil
		}
	}
	hello, err := start(ctx, req.Name, binary)
	if err != nil {
		return nil, &BuildError{Stage: "starting the worker", Err: err}
	}
	if hello.CollectorVersion != p.collectorVersion || !slices.Equal(hello.Modules, m.Modules()) {
		return nil, &BuildError{Stage: "starting the worker", Err: fmt.Errorf("the worker announced Collector %s with %d modules, the manifest names %s with %d", hello.CollectorVersion, len(hello.Modules), p.collectorVersion, len(m.Modules()))}
	}
	labels, err := runtimeLabels(req, p, goSum, GoImage)
	if err != nil {
		return nil, err
	}
	log("built " + req.Name + " " + digest)
	ok = true
	return &NativeRuntime{Name: req.Name, Path: binary, Digest: digest, Size: size, Hello: hello, Labels: labels}, nil
}

// nativeRun runs the steps of one build in directories under dir: of main,
// for goos and goarch.
type nativeRun struct {
	n            *Native
	d            nativeDirs
	req          BuildRequest
	log          func(string)
	dir          string
	main         string
	goos, goarch string
}

// resolve extracts the build module and resolves its requirements, returning
// the go.mod and go.sum that result.
func (r *nativeRun) resolve(ctx context.Context, archive, goMod []byte) ([]byte, []byte, error) {
	const stage = "resolving modules"
	dir, err := r.extract(archive)
	if err != nil {
		return nil, nil, &BuildError{Stage: stage, Err: err}
	}
	defer os.RemoveAll(dir)
	if err := r.step(ctx, stage, dir, goMod, true, resolveArgsFor(r.main)); err != nil {
		return nil, nil, err
	}
	mod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return nil, nil, &BuildError{Stage: stage, Err: errors.New("the resolution wrote no go.mod")}
	}
	sum, err := os.ReadFile(filepath.Join(dir, "go.sum"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, nil, &BuildError{Stage: stage, Err: err}
	}
	return mod, sum, nil
}

// compile extracts the build module, generates the feature gates' owners
// and compiles the worker into out, marked unverified unless verified.
func (r *nativeRun) compile(ctx context.Context, archive, goMod []byte, out string, verified bool) error {
	const stage = "compiling"
	dir, err := r.extract(archive)
	if err != nil {
		return &BuildError{Stage: stage, Err: err}
	}
	defer os.RemoveAll(dir)
	return r.step(ctx, stage, dir, goMod, false, genArgs, compileArgs(out, verified))
}

// extract unpacks a build module into a new directory. The files get the
// time of extraction, as tar -m gives them in the Go image: the archive's
// fixed times would let Go's file cache hand a build another build's file of
// the same path and size.
func (r *nativeRun) extract(archive []byte) (string, error) {
	dir, err := os.MkdirTemp(r.dir, "src-")
	if err != nil {
		return "", err
	}
	if err := extractModule(archive, dir); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

// step runs go commands in dir for the build's target, see stepFor.
func (r *nativeRun) step(ctx context.Context, stage, dir string, goMod []byte, discardStdout bool, commands ...[]string) error {
	return r.stepFor(ctx, stage, dir, goMod, discardStdout, r.goos, r.goarch, commands...)
}

// stepFor runs go commands in dir for goos and goarch one after the other
// until one fails, logging their output; discardStdout drops standard
// output (the resolve step's, which only lists packages). A failed step
// lists the packages Go could not load. Cancelling ctx kills the command's
// process group.
func (r *nativeRun) stepFor(ctx context.Context, stage, dir string, goMod []byte, discardStdout bool, goos, goarch string, commands ...[]string) error {
	logs := &lineLog{log: r.log}
	run := func(args []string, stdout, stderr io.Writer) error {
		cmd := r.n.goCommand(ctx, args...)
		cmd.Dir = dir
		cmd.Env = r.n.env(r.d, r.req.ModuleProxy, r.req.Offline, goos, goarch)
		cmd.Stdout, cmd.Stderr = stdout, stderr
		ownGroup(cmd)
		cmd.WaitDelay = 10 * time.Second
		return cmd.Run()
	}
	var err error
	for _, args := range commands {
		var stdout io.Writer = logs
		if discardStdout {
			stdout = nil
		}
		if err = run(args, stdout, logs); err != nil {
			break
		}
	}
	if err == nil {
		logs.flush()
		return nil
	}
	if ctx.Err() != nil {
		logs.flush()
		return &BuildError{Stage: stage, Output: logs.tail(), Err: context.Cause(ctx)}
	}
	// The packages go could not load are listed after the failure, as the
	// Docker step does.
	_ = run(missingArgsFor(r.main), logs, nil)
	logs.flush()
	be := &BuildError{Stage: stage, Output: logs.tail(), Err: err, Missing: missingModules(logs.tail(), goMod)}
	if errors.Is(err, exec.ErrNotFound) {
		be.Err = fmt.Errorf("the go command is not available: %w", err)
	} else if r.req.Offline && len(be.Missing) > 0 {
		be.Err = fmt.Errorf("the build is offline (GOPROXY=off) and the module cache lacks modules: %w", err)
	}
	return be
}

// extractModule unpacks the gzipped tar writeArchive makes into dir.
func extractModule(archive []byte, dir string) error {
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := h.Name
		if strings.Contains(name, "\\") || !fs.ValidPath(name) {
			return fmt.Errorf("build module entry %q is not a relative path", name)
		}
		target := filepath.Join(dir, filepath.FromSlash(name))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				_ = f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		default:
			return fmt.Errorf("build module entry %q is not a regular file or directory", name)
		}
	}
}

// hashFile returns "sha256:<hex>" and the size of a file, reading it once.
func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), n, nil
}
