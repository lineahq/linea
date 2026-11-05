package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// runtimeRegisterCmd registers a runtime in a server's catalog: it starts the
// image's worker with this machine's Docker only to read its hello, and sends
// the name, image, digest and hello to the server, which runs no container. A
// worker of the image that connects confirms it.
func runtimeRegisterCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("runtime register", "IMAGE --name NAME --server URL [--pull]", stderr)
	server := serverFlag(fs)
	name := fs.String("name", "", "the runtime name requests select on the server")
	pull := fs.Bool("pull", false, "pull the image first; otherwise it must already be in Docker's store")
	pos, code, ok := parseMixed(fs, args, 1)
	if !ok {
		return code
	}
	switch {
	case *name == "":
		return fail(stderr, errors.New("register needs --name, the runtime name requests select"))
	case *server == "":
		return fail(stderr, errors.New("register needs --server: the catalog lives on a team server"))
	}
	c, err := connect(*server, false)
	if err != nil {
		return fail(stderr, err)
	}
	local := platform.New(platform.Options{Paths: platform.PathPolicy{Unrestricted: true}, Runtimes: dockerRuntimes()})
	defer local.Close()
	img, err := local.ReadRuntimeImage(ctx, cliCaller, pos[0], *pull)
	if err != nil {
		return failRuntime(stderr, err)
	}
	rt, err := c.RegisterRuntime(ctx, api.RuntimeRegisterRequest{Name: *name, Image: img.Image, Digest: img.Digest, Hello: img.Hello})
	if err != nil {
		return failRuntime(stderr, err)
	}
	writeRegistered(stdout, rt)
	return exitOK
}

// writeRegistered states the registered runtime and whether a worker of it
// is connected yet.
func writeRegistered(w io.Writer, rt *api.Runtime) {
	cat := rt.Catalog
	if cat == nil {
		fmt.Fprintf(w, "registered runtime %s (Collector %s)\n", rt.Name, rt.CollectorVersion)
		return
	}
	fmt.Fprintf(w, "registered runtime %s (identity %s, Collector %s)\n", rt.Name, shortImageID(cat.Identity), rt.CollectorVersion)
	if cat.Workers == 0 {
		fmt.Fprintf(w, "no worker of it is connected: runs on %s queue until one starts with linea-worker --connect from image %s\n", rt.Name, cat.Image)
		return
	}
	fmt.Fprintf(w, "%d worker(s) connected, %d slot(s)\n", cat.Workers, cat.Slots)
}
