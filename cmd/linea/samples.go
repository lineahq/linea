package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/lineahq/linea/samples"
)

// samplesCmd lists the shipped samples, from this binary
// or a team server; --format json prints what GET /api/v1/samples sends.
// "samples get ID" writes one out to run it from disk.
func samplesCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "get" {
		return samplesGetCmd(args[1:], stdout, stderr)
	}
	fs := newFlagSet("samples", "[--format text|json] [--server URL] | get ID [--dir DIR]", stderr)
	format := formatFlag(fs, "text")
	server := serverFlag(fs)
	if code, ok := parse(fs, args, format); !ok {
		return code
	}
	var raw []byte
	if *server != "" {
		c, err := connect(*server, false)
		if err != nil {
			return fail(stderr, err)
		}
		if raw, err = c.Samples(ctx); err != nil {
			return fail(stderr, err)
		}
	} else {
		list, err := samples.List()
		if err != nil {
			return fail(stderr, err)
		}
		if raw, err = encoded(list); err != nil {
			return fail(stderr, err)
		}
	}
	var err error
	if *format == "json" {
		_, err = stdout.Write(raw)
	} else {
		var list []samples.Sample
		if err = json.Unmarshal(raw, &list); err == nil {
			tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "SAMPLE\tEXPECT\tTITLE")
			for _, s := range list {
				fmt.Fprintf(tw, "%s\t%s\t%s\n", s.ID, s.Expect, s.Title)
			}
			if err = tw.Flush(); err == nil {
				_, err = fmt.Fprintln(stdout, "\nwrite one to disk with: linea samples get <sample>")
			}
		}
	}
	if err != nil {
		return fail(stderr, err)
	}
	return exitOK
}

// samplesGetCmd writes one sample's configuration, input and scenario to a
// directory, the sample's ID by default, and prints the commands that run
// it. It never overwrites a file.
func samplesGetCmd(args []string, stdout, stderr io.Writer) int {
	fset := newFlagSet("samples get", "ID [--dir DIR]", stderr)
	dir := fset.String("dir", "", "directory to write the sample into (default: the sample's ID)")
	// The ID comes first; flags may follow it.
	var id string
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		id, args = args[0], args[1:]
	}
	if code, ok := parse(fset, args, nil); !ok {
		return code
	}
	if id == "" {
		fmt.Fprintln(stderr, "name the sample to write; 'linea samples' lists them")
		fset.Usage()
		return exitError
	}
	s, contents, found, err := samples.Files(id)
	if err != nil {
		return fail(stderr, err)
	}
	if !found {
		fmt.Fprintf(stderr, "no sample %q; 'linea samples' lists them\n", id)
		return exitError
	}
	if *dir == "" {
		*dir = id
	}
	paths := make([]string, len(samples.FileNames))
	for i, name := range samples.FileNames {
		paths[i] = filepath.Join(*dir, name)
		if _, err := os.Stat(paths[i]); err == nil {
			return fail(stderr, fmt.Errorf("%s already exists; remove it or choose another --dir", paths[i]))
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fail(stderr, err)
		}
	}
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		return fail(stderr, err)
	}
	for i, p := range paths {
		if err := os.WriteFile(p, contents[i], 0o644); err != nil {
			return fail(stderr, err)
		}
	}
	config, scenario, input := paths[0], paths[2], paths[1]
	fmt.Fprintf(stdout, "wrote %s, %s and %s\n\n%s\n%s\n\n", config, input, scenario, s.Title, s.Description)
	fmt.Fprintf(stdout, "check its assertions:\n  linea test --config %s --scenario %s\n", config, scenario)
	fmt.Fprintf(stdout, "see what every stage did:\n  linea run --config %s --scenario %s\n", config, scenario)
	if s.Expect == "fail" {
		fmt.Fprintln(stdout, "\nThis sample's assertions fail as shipped: find the bug and fix the configuration until test passes.")
	}
	return exitOK
}
