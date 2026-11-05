package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/lineahq/linea/internal/debugger/api"
)

// exportCmd writes a run of a team server as a reviewable scenario file. The
// server returns the file; this command writes it and never overwrites one.
func exportCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("export", "--server URL --run <run-id> --out <scenario.yaml> [--name <name>]", stderr)
	var runID, out string
	var req api.ExportRequest
	server := serverFlag(fs)
	fs.StringVar(&runID, "run", "", "ID of the run to export")
	fs.StringVar(&out, "out", "", "scenario file to create; an existing file is never overwritten")
	fs.StringVar(&req.Name, "name", "", "scenario name (default: the run's own)")
	if code, ok := parse(fs, args, nil); !ok {
		return code
	}
	if *server == "" || runID == "" || out == "" {
		fmt.Fprintln(stderr, "--server, --run and --out are required")
		fs.Usage()
		return exitError
	}
	c, err := connect(*server, false)
	if err != nil {
		return fail(stderr, err)
	}
	if _, err := os.Lstat(out); err == nil {
		return fail(stderr, fmt.Errorf("--out %s exists; export never overwrites a file", out))
	}
	exp, err := c.Export(ctx, runID, req)
	if err != nil {
		return fail(stderr, err)
	}
	if err := writeNew(out, exp.File); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "wrote %s from run %s\n", out, exp.RunID)
	if len(exp.RequiredEnvironment) > 0 {
		fmt.Fprintf(stdout, "a rerun needs environment values %s (names only, never exported)\n", strings.Join(exp.RequiredEnvironment, ", "))
	}
	fmt.Fprintln(stdout, "its inline inputs are recorded telemetry; review the file before sharing it")
	return exitOK
}

// writeNew creates path exclusively, so a file created meanwhile is not
// overwritten, and removes a partial file on failure.
func writeNew(path, content string) (err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("--out %s exists; it is never overwritten", path)
		}
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(path)
		}
	}()
	_, err = io.WriteString(f, content)
	return err
}
