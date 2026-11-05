package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestBuildRefusesIncompleteFlags: build needs a manifest, and --go-image
// only applies to a Docker build; neither starts a build.
func TestBuildRefusesIncompleteFlags(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"build"}, "--manifest is required"},
		{[]string{"build", "--manifest", "m.yaml", "--go-image", "golang:1"}, "--go-image needs --docker"},
		{[]string{"build", "--manifest", "m.yaml", "--os", "plan9"}, "linux, darwin or windows"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(context.Background(), tc.args, &stdout, &stderr); code != exitError || !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("exit %d, stderr %q; want %q", code, stderr.String(), tc.want)
			}
		})
	}
}

// TestVersionShowsACustomBuild: a stamped CLI says so in both formats,
// and a release CLI's JSON has no build key.
func TestVersionShowsACustomBuild(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"version", "--format", "json"}, &stdout, &stderr); code != exitOK || strings.Contains(stdout.String(), `"build"`) {
		t.Fatalf("release version JSON: exit %d %s", code, stdout.String())
	}

	old := customBuild
	customBuild = &buildStamp{Modules: []stampModule{{Module: "example.com/proc", Source: "/src/proc", Commit: "abc"}}}
	t.Cleanup(func() { customBuild = old })
	stdout.Reset()
	if code := run(context.Background(), []string{"version"}, &stdout, &stderr); code != exitOK ||
		!strings.Contains(stdout.String(), "custom build with linea build, unverified Collector release\n  source example.com/proc from /src/proc at abc\ncomponents:\n") {
		t.Fatalf("custom version text: exit %d\n%s", code, stdout.String())
	}
	stdout.Reset()
	if code := run(context.Background(), []string{"version", "--format", "json"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var v struct {
		Collector string     `json:"collector_version"`
		Build     buildStamp `json:"build"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.Collector == "" || v.Build.Verified || len(v.Build.Modules) != 1 || v.Build.Modules[0].Commit != "abc" {
		t.Fatalf("custom version JSON: %s", stdout.String())
	}
}
