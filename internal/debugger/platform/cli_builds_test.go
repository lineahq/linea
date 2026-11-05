package platform

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/lineahq/linea/internal/debugger/container"
	"github.com/lineahq/linea/internal/debugger/store/memory"
	"github.com/lineahq/linea/internal/debugger/workersrc"
)

// stubCLIBuild replaces the source and the compiler of CLI builds for one
// test; the compiler records its request and returns built or err.
func stubCLIBuild(t *testing.T, src fs.FS, srcErr error, built *container.CLI, err error) *container.CLIRequest {
	t.Helper()
	got := new(container.CLIRequest)
	oldSource, oldBuilder := cliSource, cliBuilder
	cliSource = func() (fs.FS, error) { return src, srcErr }
	cliBuilder = func(_ context.Context, _ BuildCLIRequest, creq container.CLIRequest) (*container.CLI, error) {
		*got = creq
		return built, err
	}
	t.Cleanup(func() { cliSource, cliBuilder = oldSource, oldBuilder })
	return got
}

// cliBuildSource is a worker source container.Check accepts for the
// built-in manifest.
func cliBuildSource(t *testing.T) fstest.MapFS {
	t.Helper()
	src := fstest.MapFS{"cmd/linea/main.go": {Data: []byte("package main\n")}}
	for _, name := range []string{"go.mod", "go.sum"} {
		data, err := os.ReadFile("../../../" + name)
		if err != nil {
			t.Fatal(err)
		}
		src[name] = &fstest.MapFile{Data: data}
	}
	return src
}

func builtInManifestPath(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("../../../build/linea-runtime.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestBuildCLIRefusesWhatItCannotBuild checks the refusals before anything
// compiles: a team server, a restricted path policy, a missing output, a
// directory output, an unknown platform, an invalid manifest and a
// debugger without the worker source.
func TestBuildCLIRefusesWhatItCannotBuild(t *testing.T) {
	stubCLIBuild(t, cliBuildSource(t), nil, nil, errors.New("must not compile"))
	dir := t.TempDir()
	ok := BuildCLIRequest{Manifest: Source{Path: builtInManifestPath(t)}, Out: filepath.Join(dir, "linea")}
	direct := func(t *testing.T) *Service {
		s := New(Options{Paths: PathPolicy{Unrestricted: true}})
		t.Cleanup(s.Close)
		return s
	}
	tests := []struct {
		name    string
		service func(t *testing.T) *Service
		req     func(BuildCLIRequest) BuildCLIRequest
		code    Code
	}{
		{"team server", func(t *testing.T) *Service {
			return openPlatform(t, memory.NewData(), Options{Paths: PathPolicy{Unrestricted: true}})
		}, nil, CodeInvalidRequest},
		{"restricted paths", func(t *testing.T) *Service {
			s := New(Options{Paths: PathPolicy{Roots: []string{dir}}})
			t.Cleanup(s.Close)
			return s
		}, nil, CodeInvalidRequest},
		{"no output", direct, func(r BuildCLIRequest) BuildCLIRequest { r.Out = ""; return r }, CodeInvalidRequest},
		{"directory output", direct, func(r BuildCLIRequest) BuildCLIRequest { r.Out = dir; return r }, CodeInvalidRequest},
		{"unknown os", direct, func(r BuildCLIRequest) BuildCLIRequest { r.GOOS = "plan9"; return r }, CodeInvalidRequest},
		{"unknown arch", direct, func(r BuildCLIRequest) BuildCLIRequest { r.GOARCH = "386"; return r }, CodeInvalidRequest},
		{"invalid manifest", direct, func(r BuildCLIRequest) BuildCLIRequest {
			r.Manifest = Source{Text: "dist: ["}
			return r
		}, CodeValidationFailed},
		{"unsupported release", direct, func(r BuildCLIRequest) BuildCLIRequest {
			r.Manifest = Source{Text: "dist:\n  otelcol_version: 0.100.0\n"}
			return r
		}, CodeValidationFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := ok
			if tt.req != nil {
				req = tt.req(req)
			}
			_, err := tt.service(t).BuildCLI(context.Background(), testCaller, req)
			wantCode(t, err, tt.code)
		})
	}
	t.Run("no worker source", func(t *testing.T) {
		stubCLIBuild(t, nil, workersrc.ErrNotBuilt, nil, errors.New("must not compile"))
		_, err := direct(t).BuildCLI(context.Background(), testCaller, ok)
		wantCode(t, err, CodeUnavailable)
	})
}

// TestBuildCLIHandsTheCheckedManifestToTheCompiler builds the built-in
// manifest for another platform with a stubbed compiler, and maps the
// compiler's failures.
func TestBuildCLIHandsTheCheckedManifestToTheCompiler(t *testing.T) {
	out := filepath.Join(t.TempDir(), "linea")
	req := BuildCLIRequest{Manifest: Source{Path: builtInManifestPath(t)}, Out: out, GOOS: "linux", GOARCH: "arm64", Offline: true}
	s := New(Options{Paths: PathPolicy{Unrestricted: true}})
	t.Cleanup(s.Close)

	got := stubCLIBuild(t, cliBuildSource(t), nil, &container.CLI{Path: out, Digest: "sha256:00", Size: 1, CollectorVersion: "0.162.0", Verified: true, GOOS: "linux", GOARCH: "arm64"}, nil)
	built, err := s.BuildCLI(context.Background(), testCaller, req)
	if err != nil {
		t.Fatal(err)
	}
	if got.GOOS != "linux" || got.GOARCH != "arm64" || !got.Offline || got.Set == nil || !strings.Contains(string(got.Manifest), "otelcol_version") {
		t.Fatalf("the compiler got %+v", got)
	}
	if built.Path != out || !built.Verified || built.Checked {
		t.Fatalf("built %+v", built)
	}

	for _, tc := range []struct {
		name string
		err  error
		code Code
	}{
		{"compile failure", &container.BuildError{What: "CLI", Stage: "compiling", Err: errors.New("exit status 1")}, CodeValidationFailed},
		{"no docker", container.ErrUnavailable, CodeUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubCLIBuild(t, cliBuildSource(t), nil, nil, tc.err)
			_, err := s.BuildCLI(context.Background(), testCaller, req)
			wantCode(t, err, tc.code)
		})
	}
}
