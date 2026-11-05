package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// runtimeDefaultCmd reads or chooses a server's default runtime: the runtime of
// every request that names none. The choice lives in the team server's
// repository, so there is no local form.
func runtimeDefaultCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("runtime default", "[NAME] --server URL [--format text|json]", stderr)
	format := formatFlag(fs, "text")
	server := serverFlag(fs)
	pos, code, ok := parseMixed(fs, args, -1)
	if !ok {
		return code
	}
	switch {
	case len(pos) > 1:
		return fail(stderr, errors.New("default takes at most one argument, the runtime to choose"))
	case *format != "text" && *format != "json":
		return fail(stderr, fmt.Errorf("unknown --format %q: want text or json", *format))
	case *server == "":
		return fail(stderr, errors.New("default needs --server: the default runtime lives on a team server"))
	}
	c, err := connect(*server, false)
	if err != nil {
		return fail(stderr, err)
	}
	var d *api.RuntimeDefault
	if len(pos) == 1 {
		d, err = c.SetDefaultRuntime(ctx, pos[0])
	} else {
		d, err = c.GetDefaultRuntime(ctx)
	}
	if err != nil {
		return failRuntime(stderr, err)
	}
	if *format == "json" {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		err = enc.Encode(d)
	} else {
		err = writeRuntimeDefault(stdout, d)
	}
	if err != nil {
		return fail(stderr, err)
	}
	return exitOK
}

// writeRuntimeDefault states the default runtime and who chose it and when;
// before a choice it is the built-in runtime or the one the server started
// with.
func writeRuntimeDefault(w io.Writer, d *api.RuntimeDefault) error {
	if _, err := fmt.Fprintf(w, "default runtime: %s\n", d.Name); err != nil {
		return err
	}
	var err error
	switch {
	case d.Set != nil:
		_, err = fmt.Fprintf(w, "chosen by %s at %s\n", d.SetBy, d.Set.UTC().Format(time.RFC3339))
	case d.Name == platform.BuiltInRuntime:
		_, err = fmt.Fprintf(w, "none was chosen: requests that name no runtime run on the built-in runtime\n")
	default:
		_, err = fmt.Fprintf(w, "none was chosen: the server started with it\n")
	}
	return err
}
