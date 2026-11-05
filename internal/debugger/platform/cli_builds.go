package platform

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"

	"github.com/lineahq/linea/internal/debugger/authz"
	"github.com/lineahq/linea/internal/debugger/container"
	"github.com/lineahq/linea/internal/debugger/workersrc"
)

// BuildCLIRequest builds the linea CLI with a manifest's components on this
// machine: a standalone CLI whose direct commands run the manifest's
// Collector release and components in process.
type BuildCLIRequest struct {
	// Manifest is the OCB manifest: a path under the path policy, text or a
	// saved manifest revision.
	Manifest Source
	// ManifestDir is the directory an inline manifest's relative local
	// directories resolve against; a manifest file's own directory is used
	// otherwise.
	ManifestDir string
	// Sources supply modules' sources beside the manifest.
	Sources []SourceOverride
	// Out is the file the CLI is written to, replaced only once the build
	// succeeded.
	Out string
	// GOOS and GOARCH are the CLI's platform: linux, darwin or windows on
	// amd64 or arm64; empty means this machine's.
	GOOS, GOARCH string
	// Docker compiles in the pinned Go image instead of with this
	// machine's go command, which must otherwise be container.GoVersion.
	Docker bool
	// GoImage overrides the pinned Go image of a Docker build.
	GoImage string
	// ModuleProxy is GOPROXY for this build; empty passes the debugger's
	// module environment through.
	ModuleProxy string
	// Offline builds from the module cache only, without network access.
	Offline bool
	// WorkDir holds a native build's module and its Go caches; empty is
	// DefaultCLIBuildDir.
	WorkDir string
	// Log receives the build's output, line by line.
	Log func(line string)
}

// CLIBuild is a built CLI.
type CLIBuild struct {
	// Path is the absolute path of the binary; Digest is "sha256:<hex>" of
	// it and Size its length.
	Path   string
	Digest string
	Size   int64
	// CollectorVersion is the manifest's release; Verified is false for a
	// release outside the support table.
	CollectorVersion string
	Verified         bool
	GOOS, GOARCH     string
	// Checked is set when the build started the CLI, which only a build
	// for this machine can do, and it announced the manifest's Collector
	// and modules.
	Checked bool
	// Warnings name the manifest fields the builder ignores and the
	// optional features the CLI lacks.
	Warnings []string
}

// DefaultCLIBuildDir is where native CLI builds keep their module and Go
// caches: linea/build in the user's cache directory.
func DefaultCLIBuildDir() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "linea", "build")
}

// cliPlatforms are the platforms the embedded source holds the CLI for.
var (
	cliOSes   = []string{"linux", "darwin", "windows"}
	cliArches = []string{"amd64", "arm64"}
)

// cliBuilder compiles a CLI; tests replace it.
var cliBuilder = func(ctx context.Context, req BuildCLIRequest, creq container.CLIRequest) (*container.CLI, error) {
	if req.Docker {
		return (&container.Docker{}).BuildCLI(ctx, creq, req.Out)
	}
	work := req.WorkDir
	if work == "" {
		if work = DefaultCLIBuildDir(); work == "" {
			return nil, &Error{Code: CodeInvalidRequest, Message: "this machine has no user cache directory: pass a work directory"}
		}
	}
	native := &container.Native{WorkDir: work}
	have, err := native.Toolchain(ctx)
	if err != nil {
		return nil, &Error{Code: CodeUnavailable, Message: fmt.Sprintf("a native build needs Go %s on PATH (or build with Docker): %v", container.GoVersion, err), cause: err}
	}
	if have != container.GoVersion {
		return nil, &Error{Code: CodeUnavailable, Message: fmt.Sprintf("a native build needs Go %s, this machine's go is %s: install %s or build with Docker", container.GoVersion, have, container.GoVersion)}
	}
	return native.BuildCLI(ctx, creq, req.Out)
}

// cliSource is the worker source a CLI build compiles; tests replace it.
var cliSource = func() (fs.FS, error) { return workersrc.Embedded() }

// BuildCLI compiles the linea CLI with a manifest's components and writes
// it to req.Out, waiting for the build. Only a direct composition (the CLI,
// which reads and writes this machine's files) builds CLIs; elsewhere it is
// invalid_request. A manifest refused before compiling is validation_failed
// with one diagnostic per problem; a debugger without the worker source, or
// without a usable Go toolchain or Docker, is unavailable; a failed compile
// is validation_failed with the compiler output in the message.
func (s *Service) BuildCLI(ctx context.Context, caller Caller, req BuildCLIRequest) (_ *CLIBuild, err error) {
	defer s.audit(&caller, "BuildCLI", &err)()
	if err := s.begin(ctx, caller, authz.RuntimeManage); err != nil {
		return nil, err
	}
	if _, server := s.serverBuilds(); server || !s.policy.Paths.Direct() {
		return nil, &Error{Code: CodeInvalidRequest, Message: "a CLI is built on the machine that runs it: run linea build there"}
	}
	if req.Out == "" {
		return nil, &Error{Code: CodeInvalidRequest, Message: "a CLI build needs the file to write"}
	}
	goos, goarch := req.GOOS, req.GOARCH
	if goos == "" {
		goos = runtime.GOOS
	}
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	if !slices.Contains(cliOSes, goos) || !slices.Contains(cliArches, goarch) {
		return nil, &Error{Code: CodeInvalidRequest, Message: fmt.Sprintf("a CLI builds for linux, darwin or windows on amd64 or arm64, not %s/%s", goos, goarch)}
	}
	if info, err := os.Stat(req.Out); err == nil && info.IsDir() {
		return nil, &Error{Code: CodeInvalidRequest, Message: fmt.Sprintf("%s is a directory: name the file to write", req.Out)}
	}
	in, err := s.buildInputs(ctx, caller, req.Manifest, req.ManifestDir, req.Sources)
	if err != nil {
		return nil, err
	}
	src, err := cliSource()
	if errors.Is(err, workersrc.ErrNotBuilt) {
		return nil, &Error{Code: CodeUnavailable, Message: err.Error(), cause: err}
	}
	if err != nil {
		return nil, err
	}
	creq := container.CLIRequest{
		Manifest: in.raw, Source: src, Set: dependencySet(in.release), Sources: moduleSources(in.sources),
		GOOS: goos, GOARCH: goarch, ModuleProxy: req.ModuleProxy, Offline: req.Offline, GoImage: req.GoImage, Log: req.Log,
	}
	if _, err := container.Check(src, in.raw, creq.Set, creq.Sources); err != nil {
		var modules *container.ModuleError
		if errors.As(err, &modules) {
			return nil, classify(manifestDiagnostics("manifest_modules", modules.Problems...), CodeValidationFailed)
		}
		return nil, classify(manifestDiagnostics("invalid_manifest", err.Error()), CodeValidationFailed)
	}
	built, err := cliBuilder(ctx, req, creq)
	if err != nil {
		return nil, cliBuildError(ctx, err)
	}
	return &CLIBuild{
		Path: built.Path, Digest: built.Digest, Size: built.Size, CollectorVersion: built.CollectorVersion, Verified: built.Verified,
		GOOS: built.GOOS, GOARCH: built.GOARCH, Checked: built.Checked, Warnings: in.warnings("CLI"),
	}, nil
}

// cliBuildError maps a CLI build's failure to a platform error.
func cliBuildError(ctx context.Context, err error) error {
	var perr *Error
	if errors.As(err, &perr) {
		return err
	}
	if ctx.Err() != nil {
		return classify(ctx.Err(), CodeCancelled)
	}
	var be *container.BuildError
	if errors.As(err, &be) {
		return &Error{Code: CodeValidationFailed, Message: be.Error(), cause: err}
	}
	if errors.Is(err, container.ErrUnavailable) {
		return &Error{Code: CodeUnavailable, Message: err.Error(), cause: err}
	}
	return err
}
