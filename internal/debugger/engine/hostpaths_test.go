package engine

import (
	"errors"
	goruntime "runtime"
	"testing"

	"github.com/lineahq/linea/internal/debugger/scenario"
)

// TestHostPathsFollowTheHostsRules resolves scenario file names as a Windows
// or Unix host captured them, whatever system resolves them: a Windows
// host's bundle executes in a Linux worker container.
func TestHostPathsFollowTheHostsRules(t *testing.T) {
	for _, c := range []struct{ os, dir, name, want string }{
		{"windows", `F:\otel\samples\logs`, "input.json", `F:\otel\samples\logs\input.json`},
		{"windows", `F:\otel\samples\logs`, "data/in.json", `F:\otel\samples\logs\data\in.json`},
		{"windows", `F:\otel\samples\logs`, `..\shared\.\in.json`, `F:\otel\samples\shared\in.json`},
		{"windows", `F:\otel\samples\logs`, `C:\data\in.json`, `C:\data\in.json`},
		{"windows", `F:\otel\samples\logs`, `C:/data/in.json`, `C:\data\in.json`},
		{"windows", `F:\otel\samples\logs`, `\data\in.json`, `F:\otel\samples\logs\data\in.json`},
		{"windows", `\\host\share\logs`, `in.json`, `\\host\share\logs\in.json`},
		{"windows", "", `C:\data\in.json`, `C:\data\in.json`},
		{"linux", "/srv/samples/logs", "input.json", "/srv/samples/logs/input.json"},
		{"linux", "/srv/samples/logs", "../shared/./in.json", "/srv/samples/shared/in.json"},
		{"darwin", "/srv/samples/logs", "/data/in.json", "/data/in.json"},
	} {
		var got string
		var err error
		if c.os == "windows" {
			got, err = windowsAbs(c.dir, c.name)
		} else {
			got, err = slashAbs(c.dir, c.name)
		}
		if err != nil || got != c.want {
			t.Errorf("%s: %q in %q = %q, %v; want %q", c.os, c.name, c.dir, got, err, c.want)
		}
		if c.os == goruntime.GOOS || c.os != "windows" && goruntime.GOOS != "windows" {
			// On the host's own system the result is filepath.Abs's.
			if native, err := hostAbs("", c.dir, c.name); err != nil || native != c.want {
				t.Errorf("filepath on %s: %q in %q = %q, %v; want %q", goruntime.GOOS, c.name, c.dir, native, err, c.want)
			}
		}
	}
	for _, os := range []string{"windows", "linux"} {
		if _, err := hostAbs(os, "", "input.json"); goruntime.GOOS != os && !errors.Is(err, scenario.ErrRelativeWithoutDir) {
			t.Errorf("%s: a relative name without a directory: %v", os, err)
		}
	}
	if _, err := hostAbs("", "", "input.json"); !errors.Is(err, scenario.ErrRelativeWithoutDir) {
		t.Errorf("native: a relative name without a directory: %v", err)
	}
}
