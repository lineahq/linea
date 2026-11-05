package platform

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/lineahq/linea/internal/debugger/container"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/worker"
)

// Runtime images from a registry: a direct command names an
// image reference where it names a runtime, and the store reads the image
// without Docker and runs its binary as a child process, as a pool runs a
// built runtime. A team server runs only the runtimes of its catalog.

// imageStore is a RuntimeStore that reads runtime images from a registry.
type imageStore interface {
	// PullImage returns the runtime of an image reference, with Binary
	// set when its binary runs natively on this host.
	PullImage(ctx context.Context, ref string) (*RuntimeInfo, error)
}

// bindImage returns the started worker of a runtime image, reading the
// image on first use.
func (s *Service) bindImage(ctx context.Context, ref string) (*boundRuntime, error) {
	if s.workerToken != "" {
		return nil, &Error{Code: CodeInvalidRequest, Message: fmt.Sprintf(
			"runtime %s is an image reference: a team server runs the runtimes of its catalog; name one that linea runtime list --server shows", ref)}
	}
	store, err := s.runtimeStore()
	if err != nil {
		return nil, err
	}
	var info *RuntimeInfo
	if images, ok := store.(imageStore); ok {
		info, err = images.PullImage(ctx, ref)
	} else {
		info, err = store.Get(ctx, ref)
	}
	switch {
	case errors.Is(err, container.ErrNotRuntime):
		return nil, &Error{Code: CodeInvalidRequest, Message: err.Error(), cause: err}
	case err != nil:
		return nil, &Error{Code: CodeUnavailable, Message: fmt.Sprintf("runtime %s is not available: %v", ref, err), cause: err}
	}
	return s.start(ctx, store, info)
}

// PullImage reads ref into the image cache. An image this host cannot run
// natively runs in Docker when Docker has it.
func (d *dockerRuntimes) PullImage(ctx context.Context, ref string) (*RuntimeInfo, error) {
	if d.opts.ImageCache == "" {
		return d.Get(ctx, ref)
	}
	auth, err := container.DockerConfig()
	if err != nil {
		return nil, fmt.Errorf("reading the Docker client's configuration: %w", err)
	}
	img, err := (&container.ImagePull{Dir: d.opts.ImageCache, Auth: auth, Platform: imagePlatform}).Pull(ctx, ref)
	if errors.Is(err, container.ErrPlatform) {
		if info, gerr := d.Get(ctx, ref); gerr == nil {
			return info, nil
		}
		return nil, fmt.Errorf("%w; to run it in Docker instead, run linea runtime import --pull %s", err, ref)
	}
	if err != nil {
		return nil, err
	}
	info := runtimeInfo(container.Image{ID: img.Digest, Labels: img.Labels, Size: img.Size})
	info.Reference, info.Binary = img.Reference, img.Path
	return &info, nil
}

// imagePlatform is the platform of the images a pull accepts; empty is the
// host's. Tests replace it, as they replace binaryCommand, to run the
// binary on a host that cannot execute it by its cached name.
var imagePlatform string

var binaryCommand = exec.Command

// binaryEnv is the whole environment of a runtime binary: none of the
// host's, as a runtime container has none. Tests replace it.
var binaryEnv = func() []string { return []string{"TMPDIR=" + os.TempDir()} }

// startBinary runs a runtime image's binary as a stdin/stdout worker and
// checks that it is the Collector its labels name.
func startBinary(ctx context.Context, info *RuntimeInfo) (RuntimeWorker, error) {
	cmd := binaryCommand(info.Binary)
	cmd.Env = binaryEnv()
	cmd.WaitDelay = 5 * time.Second
	c, err := worker.Start(ctx, info.Name, cmd)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", engine.ErrRuntimeUnavailable, err)
	}
	if h := c.Hello(); h.CollectorVersion != info.CollectorVersion {
		_ = c.Close()
		return nil, fmt.Errorf("%w: runtime image %s runs Collector %s, and its labels say %s",
			engine.ErrRuntimeUnavailable, info.Reference, h.CollectorVersion, info.CollectorVersion)
	}
	return c, nil
}

// DefaultImageCache is where direct commands keep the binaries of runtime
// images: the user's cache directory, or none when the host has none.
func DefaultImageCache() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "linea", "runtimes")
}

// PruneImageCache removes every runtime binary cached in dir and returns
// how many images it removed.
func PruneImageCache(dir string) (int, error) {
	return container.PruneImages(dir)
}
