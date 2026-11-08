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
	"strings"

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
			err = writeSamples(stdout, list)
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
	if err := writeSampleGet(stdout, s, *dir, paths[0], paths[1], paths[2]); err != nil {
		return fail(stderr, err)
	}
	return exitOK
}

// writeSamples prints one row per sample and how to write one to disk.
func writeSamples(w io.Writer, list []samples.Sample) error {
	t := newTextOut(w)
	if len(list) == 0 {
		t.none("samples")
	}
	rows := make([][]string, 0, len(list))
	for _, s := range list {
		rows = append(rows, []string{s.ID, s.Expect, s.Title})
	}
	t.table(0, []string{"sample", "expect", "title"}, rows)
	if len(list) > 0 {
		t.printf("\n")
		t.line(0, "write one to disk with: linea samples get <sample>")
	}
	return t.err()
}

// writeSampleGet prints what samples get wrote and the commands that run it.
func writeSampleGet(w io.Writer, s samples.Sample, dir, config, input, scenario string) error {
	t := newTextOut(w)
	t.line(0, "wrote sample %s to %s", s.ID, dir)
	t.field(1, "title", s.Title)
	t.field(1, "expect", s.Expect)
	if description := strings.TrimSpace(s.Description); description != "" {
		t.heading("description")
		for _, l := range strings.Split(description, "\n") {
			if l = strings.TrimRight(l, " \r"); l == "" {
				t.printf("\n")
				continue
			}
			t.line(1, "%s", l)
		}
	}
	t.heading("files")
	t.field(1, "config", config)
	t.field(1, "input", input)
	t.field(1, "scenario", scenario)
	t.heading("next")
	t.line(1, "check its assertions: linea test --config %s --scenario %s", config, scenario)
	t.line(1, "see what every stage did: linea run --config %s --scenario %s", config, scenario)
	if s.Expect == "fail" {
		t.line(1, "this sample's assertions fail as shipped: find the bug and fix the configuration until test passes")
	}
	return t.err()
}
