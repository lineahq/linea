// Command linea-builder compiles runtimes for a team server.
//
// It dials the server with --connect, presents the builder token from
// --token-file, announces its capacity and builds the runtimes the server
// gives it: it downloads the manifest and the uploaded module sources from
// presigned URLs, compiles the worker with the Go toolchain on its PATH, and
// uploads the binary to a presigned URL with the hello the binary sent when
// started once. It listens on no port and holds no database or bucket
// credentials.
//
// The first SIGTERM or interrupt drains the builder: it takes
// no new build, finishes the builds it runs and exits. A second one
// cancels them; the server retries a cancelled build once its lease ends.
//
// The toolchain must be the release the Docker builds use
// (container.GoVersion), because only then is a runtime built here the same
// binary a CLI build produces; the builder refuses to start with another. It
// also needs the worker source built into it: run
// `go generate ./internal/debugger/workersrc` before building it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/lineahq/linea/build/versions"
	"github.com/lineahq/linea/internal/debugger/container"
	"github.com/lineahq/linea/internal/debugger/worker"
	"github.com/lineahq/linea/internal/debugger/workersrc"
)

func main() {
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	ctx, drain, cancel := stopping(context.Background(), signals, os.Stderr)
	defer cancel()
	if err := run(ctx, drain, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "linea-builder:", err)
		os.Exit(1)
	}
}

// stopping turns the first signal into a drain and the second into a
// cancellation of the returned context.
func stopping(parent context.Context, signals <-chan os.Signal, log io.Writer) (context.Context, <-chan struct{}, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	drain := make(chan struct{})
	go func() {
		select {
		case <-signals:
		case <-ctx.Done():
			return
		}
		fmt.Fprintln(log, "linea-builder: stopping: finishing the builds in progress; signal again to cancel them")
		close(drain)
		select {
		case <-signals:
		case <-ctx.Done():
			return
		}
		fmt.Fprintln(log, "linea-builder: cancelling the builds in progress")
		cancel()
	}()
	return ctx, drain, cancel
}

// options are the flags.
type options struct {
	connect, tokenFile, caFile, name, workDir string
	capacity, cpus                            int
	// registryAuth is a Docker config.json with the credentials for image
	// exports, plainHTTP the registries reached over http.
	registryAuth string
	plainHTTP    []string
}

func parseOptions(args []string) (options, error) {
	var o options
	fs := flag.NewFlagSet("linea-builder", flag.ContinueOnError)
	host, _ := os.Hostname()
	fs.StringVar(&o.connect, "connect", "", "dial the team server at this URL (required)")
	fs.StringVar(&o.tokenFile, "token-file", "", "file holding the server's build token (required)")
	fs.StringVar(&o.name, "name", host, "the builder's name in the server's logs and leases (default: the host name)")
	fs.IntVar(&o.capacity, "capacity", 1, "how many builds the builder runs at once")
	fs.StringVar(&o.caFile, "ca-file", "", "verify the server's certificate with this PEM CA bundle")
	fs.StringVar(&o.workDir, "work-dir", filepath.Join(os.TempDir(), "linea-builder"), "directory for build modules, binaries and the Go caches; it must allow executing files")
	fs.IntVar(&o.cpus, "cpus", container.DefaultBuildCPUs, "CPUs a build may use (GOMAXPROCS)")
	fs.StringVar(&o.registryAuth, "registry-auth", "", "Docker config.json with the registry credentials for image exports (auths, credsStore, credHelpers), read at each export")
	fs.Func("registry-plain-http", "reach this registry host over http, not https, for image exports (repeatable)", func(v string) error {
		if v == "" || strings.ContainsAny(v, "/ ") {
			return fmt.Errorf("%q is not a registry host", v)
		}
		o.plainHTTP = append(o.plainHTTP, v)
		return nil
	})
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	switch {
	case fs.NArg() > 0:
		return options{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	case o.connect == "":
		return options{}, errors.New("--connect is required: the team server to build for")
	case o.tokenFile == "":
		return options{}, errors.New("--token-file is required: the file holding the server's build token")
	case strings.TrimSpace(o.name) == "":
		return options{}, errors.New("--name is required when the host has no name")
	case o.capacity < 1:
		return options{}, errors.New("--capacity must be at least 1")
	case o.cpus < 1:
		return options{}, errors.New("--cpus must be at least 1")
	}
	return o, nil
}

// run serves builds until ctx ends, or until drain is closed and the
// builds in progress have ended.
func run(ctx context.Context, drain <-chan struct{}, args []string) error {
	o, err := parseOptions(args)
	if err != nil {
		return err
	}
	token, err := readToken(o.tokenFile)
	if err != nil {
		return err
	}
	var ca []byte
	if o.caFile != "" {
		if ca, err = os.ReadFile(o.caFile); err != nil {
			return fmt.Errorf("--ca-file: %w", err)
		}
	}
	native := &container.Native{WorkDir: o.workDir, CPUs: o.cpus}
	source, err := checkStart(ctx, native, workersrc.Embedded)
	if err != nil {
		return err
	}
	b := &builder{build: native.Build, source: source, table: versions.Embedded(), client: newTransferClient(),
		registryAuth: o.registryAuth, export: &container.ImageExport{Client: newTransferClient(), PlainHTTP: o.plainHTTP, WorkDir: o.workDir}}
	return worker.ServeBuilds(ctx, worker.BuilderOptions{
		URL: o.connect, CA: ca, Token: token,
		Hello: worker.BuilderHello{GoVersion: container.GoVersion, OS: "linux", Arch: runtime.GOARCH, Name: o.name, Capacity: o.capacity},
		Log:   os.Stderr, Drain: drain,
	}, b.run)
}

// toolchain reports the Go release of the go command on PATH.
type toolchain interface {
	Toolchain(ctx context.Context) (string, error)
}

// checkStart refuses to start without the worker source built in or with a
// Go toolchain other than the one the Docker builds use, and returns the
// worker source.
func checkStart(ctx context.Context, tc toolchain, embedded func() (fs.FS, error)) (fs.FS, error) {
	source, err := embedded()
	if err != nil {
		return nil, err
	}
	have, err := tc.Toolchain(ctx)
	if err != nil {
		return nil, fmt.Errorf("the go command on PATH: %w", err)
	}
	if have != container.GoVersion {
		return nil, fmt.Errorf("the go command on PATH is %s, this builder needs %s: only that toolchain builds the binary a CLI build produces", have, container.GoVersion)
	}
	return source, nil
}

func readToken(file string) (string, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("--token-file: %w", err)
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return "", errors.New("--token-file: the file is empty")
	}
	return token, nil
}
