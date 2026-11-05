package container

import (
	"context"
	"strconv"

	"github.com/lineahq/linea/internal/debugger/worker"
)

// DefaultRunMemory is a worker container's memory limit when RunOptions
// names none.
const DefaultRunMemory = 2 << 30

// RunOptions limits a worker container.
type RunOptions struct {
	// Memory is the memory limit in bytes (swap included); zero means
	// DefaultRunMemory.
	Memory int64
	// CPUs limits the container's CPUs; zero means no limit.
	CPUs float64
}

// Worker is a runtime's worker running in a container.
type Worker struct {
	*worker.Client
	docker    *Docker
	container string
}

// Start runs the worker of a runtime image (an image reference or ID) in a
// container without network access, a read-only root, a /tmp tmpfs, a
// non-root user and the memory limit, and waits for its hello. name
// identifies the runtime in errors and bundles.
func (d *Docker) Start(ctx context.Context, image, name string, opts RunOptions) (*Worker, error) {
	memory := opts.Memory
	if memory <= 0 {
		memory = DefaultRunMemory
	}
	container := "linea-worker-" + randomSuffix()
	args := []string{
		"run", "-i", "--rm", "--name", container,
		"--network", "none", "--read-only", "--tmpfs", "/tmp",
		"--user", "65534:65534", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--memory", strconv.FormatInt(memory, 10), "--memory-swap", strconv.FormatInt(memory, 10),
		"--label", LabelRuntime + "-worker=" + name,
	}
	if opts.CPUs > 0 {
		args = append(args, "--cpus", strconv.FormatFloat(opts.CPUs, 'f', -1, 64))
	}
	args = append(args, image)
	// The docker CLI is the worker process: closing its input ends the
	// worker and --rm removes the container. Not tied to ctx, which only
	// bounds the wait for the hello.
	c, err := worker.Start(ctx, name, d.command(context.Background(), args...))
	if err != nil {
		d.removeContainer(container)
		return nil, err
	}
	return &Worker{Client: c, docker: d, container: container}, nil
}

// Container is the worker's container name.
func (w *Worker) Container() string { return w.container }

// Close ends the worker and removes its container.
func (w *Worker) Close() error {
	err := w.Client.Close()
	w.docker.removeContainer(w.container)
	return err
}
