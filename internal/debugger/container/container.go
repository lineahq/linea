// Package container builds debug Collector runtimes and runs their workers
// in Docker, through the docker CLI only.
//
// A build compiles the worker's source (internal/debugger/workersrc) with a
// manifest's registry in the official Go image, with the user's module
// source and per-user cache volumes; the debugger ships no builder image and
// no modules. The worker is wrapped in a runtime image FROM scratch whose
// labels describe it, and the build counts only once that image's worker
// starts and says hello. A runtime's worker runs in a container without
// network access, with a read-only root, a /tmp tmpfs, a non-root user and a
// memory limit; it receives only captured bytes over its standard input.
package container

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// Labels of a runtime image. Docker's image store is the runtime list; the
// workspace keeps no runtime records.
const (
	LabelRuntime          = "io.linea.runtime"
	LabelProductVersion   = "io.linea.product-version"
	LabelCollectorVersion = "io.linea.collector-version"
	LabelProtocol         = "io.linea.protocol"
	LabelVerified         = "io.linea.verified"
	LabelManifest         = "io.linea.manifest"
	LabelModuleSet        = "io.linea.module-set"
	LabelGoImage          = "io.linea.go-image"
	// LabelCustomModules is a JSON array of CustomModule: the modules built
	// from supplied sources, with their origin and commit.
	LabelCustomModules = "io.linea.custom-modules"
	// LabelProviders is a JSON array of Provider: the debugger providers
	// the custom modules export.
	LabelProviders = "io.linea.providers"
)

// Repository is the image repository of runtimes; the tag is the runtime's
// name.
const Repository = "linea-runtime"

// Cache volumes shared by every build of this user: the module cache
// and the build cache. They make rebuilds fast and, once warm, offline.
const (
	ModuleCacheVolume = "linea-gomod"
	BuildCacheVolume  = "linea-gocache"
)

// ErrUnavailable means Docker cannot be used: the CLI is missing or the
// daemon does not answer.
var ErrUnavailable = errors.New("docker is not available")

// Docker runs the docker CLI.
type Docker struct {
	// Path is the docker executable; empty means "docker" on PATH.
	Path string
}

func (d *Docker) path() string {
	if d == nil || d.Path == "" {
		return "docker"
	}
	return d.Path
}

func (d *Docker) command(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, d.path(), args...)
}

// output runs a docker command and returns its trimmed standard output; an
// error carries its standard error.
func (d *Docker) output(ctx context.Context, stdin []byte, args ...string) (string, error) {
	cmd := d.command(ctx, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return "", fmt.Errorf("%w: %w", ErrUnavailable, err)
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("docker %s: %s", args[0], msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// Version returns the Docker server version, or ErrUnavailable.
func (d *Docker) Version(ctx context.Context) (string, error) {
	v, err := d.output(ctx, nil, "version", "--format", "{{.Server.Version}}")
	if err != nil {
		if errors.Is(err, ErrUnavailable) {
			return "", err
		}
		return "", fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return v, nil
}

// removeContainer removes a container by name, whatever its state. It uses
// its own context: it runs after the caller's ended.
func (d *Docker) removeContainer(name string) {
	_ = d.command(context.Background(), "rm", "-f", name).Run()
}

var runtimeName = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,62}$`)

// ValidName reports whether name can name a runtime: lower-case letters,
// digits, '_', '.' and '-', starting with a letter or digit, at most 63
// characters (an image tag).
func ValidName(name string) bool { return runtimeName.MatchString(name) }

// Ref is the image reference of the runtime called name.
func Ref(name string) string { return Repository + ":" + name }

func randomSuffix() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
