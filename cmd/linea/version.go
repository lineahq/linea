package main

import (
	"context"
	"io"
	"strings"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// customBuild is set by the custom_build.go that linea build generates
// beside this file; the release CLI has none, so it stays nil.
var customBuild *buildStamp

// buildStamp says what a CLI built with linea build was built from.
type buildStamp struct {
	// Verified is false for a Collector release outside the building
	// debugger's support table.
	Verified bool `json:"verified"`
	// Modules are the module sources the build replaced.
	Modules []stampModule `json:"custom_modules,omitempty"`
}

// stampModule is one module source of a custom build.
type stampModule struct {
	Module string `json:"module"`
	Source string `json:"source"`
	Commit string `json:"commit,omitempty"`
}

// versionOutput is version's JSON: the versions, and for a custom build
// what it was built from.
type versionOutput struct {
	platform.Versions
	Build *buildStamp `json:"build,omitempty"`
}

// versionCmd prints the debugger, Collector and schema versions.
func versionCmd(_ context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("version", "[--format text|json]", stderr)
	format := formatFlag(fs, "text")
	if code, ok := parse(fs, args, format); !ok {
		return code
	}
	v := platform.CurrentVersions()
	if *format == "json" {
		if err := platform.Encode(stdout, versionOutput{Versions: v, Build: customBuild}); err != nil {
			return fail(stderr, err)
		}
		return exitOK
	}
	t := newTextOut(stdout)
	t.line(0, "linea %s", v.Product)
	t.field(1, "collector", v.Collector)
	t.field(1, "built with", v.Go)
	t.field(1, "report schema", v.ReportSchema)
	t.field(1, "inspection schema", v.InspectionSchema)
	t.field(1, "event schema", v.EventSchema)
	if b := customBuild; b != nil {
		t.field(1, "build", "custom, made with linea build")
		if !b.Verified {
			t.field(1, "collector release", "unverified")
		}
		for _, m := range b.Modules {
			source := m.Module + " from " + m.Source
			if m.Commit != "" {
				source += " at " + m.Commit
			}
			t.field(1, "source", source)
		}
	}
	t.heading("components")
	var rows [][]string
	for _, m := range v.Modules {
		module, version, _ := strings.Cut(m, " ")
		rows = append(rows, []string{module, version})
	}
	t.table(1, []string{"module", "version"}, rows)
	if err := t.err(); err != nil {
		return fail(stderr, err)
	}
	return exitOK
}
