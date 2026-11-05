package main

import (
	"context"
	"fmt"
	"io"

	"github.com/lineahq/linea/internal/debugger/client"
)

// runs delete and suite delete remove finished history
// with the rules DELETE /runs/{id} and /suites/{id} apply: only terminal
// runs, and a suite member only with its suite. --format json prints
// {"deleted": true}, as capture delete does.

type deleted struct {
	Deleted bool `json:"deleted"`
}

func runsDeleteCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runRead(ctx, "delete", "--server URL --run ID [--format text|json]", args, stdout, stderr, nil,
		func(ctx context.Context, c *client.Client, id string) ([]byte, error) {
			if err := c.DeleteRun(ctx, id); err != nil {
				return nil, err
			}
			return encoded(deleted{true})
		},
		func(w io.Writer, _ []byte) error {
			_, err := fmt.Fprintln(w, "run deleted")
			return err
		})
}

func suiteDeleteCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("suite delete", "--server URL --suite ID [--format text|json]", stderr)
	format := formatFlag(fs, "text")
	server := serverFlag(fs)
	id := fs.String("suite", "", "the suite to delete with its member runs")
	if code, ok := parse(fs, args, format); !ok {
		return code
	}
	if *server == "" || *id == "" {
		fmt.Fprintln(stderr, "--server and --suite are required: suites are kept by linea-server")
		fs.Usage()
		return exitError
	}
	c, err := connect(*server, false)
	if err != nil {
		return fail(stderr, err)
	}
	if err := c.DeleteSuite(ctx, *id); err != nil {
		return fail(stderr, err)
	}
	if *format == "json" {
		raw, err := encoded(deleted{true})
		if err == nil {
			_, err = stdout.Write(raw)
		}
		if err != nil {
			return fail(stderr, err)
		}
		return exitOK
	}
	fmt.Fprintln(stdout, "suite and its member runs deleted")
	return exitOK
}
