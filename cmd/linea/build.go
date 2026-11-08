package main

import (
	"context"
	"fmt"
	"io"
	"runtime"

	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// buildCmd compiles this CLI with a manifest's components on this machine,
// so its direct commands run that Collector release and those components
// in process.
func buildCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("build", "--manifest <ocb.yaml> [--out FILE] [--source MODULE=DIR]... [--git MODULE=URL[#REF[:SUBDIR]]]... [--os OS] [--arch ARCH] [--docker [--go-image IMG]] [--module-proxy URL] [--offline]", stderr)
	var req platform.BuildCLIRequest
	var manifest string
	var sources sourceFlag
	fs.StringVar(&manifest, "manifest", "", "OpenTelemetry Collector Builder manifest YAML; its relative path: and replace directories are read beside it")
	fs.StringVar(&req.Out, "out", "", "the CLI file to write, replaced once the build succeeded (default linea-custom, with .exe for windows, in the current directory)")
	fs.Var(&sources, "source", "a module's source directory, holding its go.mod (repeatable); replaces the manifest's directory for that module")
	fs.Var(gitFlag{&sources}, "git", "a module's git repository, cloned with this machine's git at REF (default HEAD), the module in SUBDIR (repeatable)")
	fs.StringVar(&req.GOOS, "os", runtime.GOOS, "the CLI's operating system: linux, darwin or windows")
	fs.StringVar(&req.GOARCH, "arch", runtime.GOARCH, "the CLI's architecture: amd64 or arm64")
	fs.BoolVar(&req.Docker, "docker", false, "compile in the pinned Go image instead of with this machine's go, which must be the pinned Go release")
	fs.StringVar(&req.GoImage, "go-image", "", "with --docker, the Go image to compile in; empty is the pinned official image")
	fs.StringVar(&req.ModuleProxy, "module-proxy", "", "GOPROXY for this build; empty passes the module environment through")
	fs.BoolVar(&req.Offline, "offline", false, "build from the module cache only, without network access")
	if code, ok := parse(fs, args, nil); !ok {
		return code
	}
	if manifest == "" {
		fmt.Fprintln(stderr, "--manifest is required")
		fs.Usage()
		return exitError
	}
	if req.GoImage != "" && !req.Docker {
		fmt.Fprintln(stderr, "--go-image needs --docker")
		fs.Usage()
		return exitError
	}
	if req.Out == "" {
		req.Out = "linea-custom"
		if req.GOOS == "windows" {
			req.Out += ".exe"
		}
	}
	req.Manifest = platform.Source{Path: manifest}
	req.Sources = api.SourceOverrides(sources)
	req.Log = func(line string) { fmt.Fprintln(stderr, line) }
	service := platform.New(platform.Options{Paths: platform.PathPolicy{Unrestricted: true}})
	defer service.Close()
	built, err := service.BuildCLI(ctx, cliCaller, req)
	if err != nil {
		return failRuntime(stderr, err)
	}
	for _, w := range built.Warnings {
		fmt.Fprintln(stderr, "warning: "+w)
	}
	if !built.Verified {
		fmt.Fprintf(stderr, "note: Collector %s is outside the releases this debugger verified; the CLI is unverified\n", built.CollectorVersion)
	}
	if !built.Checked {
		fmt.Fprintf(stderr, "note: the CLI is for %s/%s, so the build did not start it; run its version command there to check it\n", built.GOOS, built.GOARCH)
	}
	t := newTextOut(stdout)
	t.line(0, "built %s", built.Path)
	t.field(1, "collector", built.CollectorVersion)
	t.field(1, "platform", built.GOOS+"/"+built.GOARCH)
	t.field(1, "size", fmt.Sprintf("%d bytes", built.Size))
	t.field(1, "digest", built.Digest)
	if err := t.err(); err != nil {
		return fail(stderr, err)
	}
	return exitOK
}
