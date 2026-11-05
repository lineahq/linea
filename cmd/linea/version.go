package main

import (
	"context"
	"fmt"
	"io"

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
	_, err := fmt.Fprintf(stdout, "linea %s\ncollector %s, built with %s\nschemas: report %s, inspection %s, events %s\n",
		v.Product, v.Collector, v.Go, v.ReportSchema, v.InspectionSchema, v.EventSchema)
	if err == nil && customBuild != nil {
		_, err = io.WriteString(stdout, customBuildLine(customBuild))
	}
	if err == nil {
		_, err = io.WriteString(stdout, "components:\n")
	}
	for _, m := range v.Modules {
		if err == nil {
			_, err = fmt.Fprintf(stdout, "  %s\n", m)
		}
	}
	if err != nil {
		return fail(stderr, err)
	}
	return exitOK
}

// customBuildLine describes a custom build in version's text.
func customBuildLine(b *buildStamp) string {
	s := "custom build with linea build"
	if !b.Verified {
		s += ", unverified Collector release"
	}
	s += "\n"
	for _, m := range b.Modules {
		s += "  source " + m.Module + " from " + m.Source
		if m.Commit != "" {
			s += " at " + m.Commit
		}
		s += "\n"
	}
	return s
}
