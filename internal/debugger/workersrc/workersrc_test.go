package workersrc

import (
	"bytes"
	"io/fs"
	"path"
	"strings"
	"testing"
)

// TestPackHoldsTheWorkersSource packs this checkout: the archive holds the
// worker's and the CLI's mains, the module files and the packages they
// import on every platform the CLI builds for, never tests, the team
// server's packages or the archive itself, and packing is deterministic.
func TestPackHoldsTheWorkersSource(t *testing.T) {
	raw, err := Pack("../../..")
	if err != nil {
		t.Fatal(err)
	}
	again, err := Pack("../../..")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, again) {
		t.Fatal("packing twice gave different archives")
	}
	src, err := Unpack(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"go.mod", "go.sum", "cmd/linea-worker/main.go", "internal/debugger/worker/server.go", "internal/debugger/engine/engine.go", "internal/debugger/registry/components.go", "internal/debugger/registry/filestorage.go",
		// Every OTTL variant and optional feature, whatever the root's manifest picks.
		"internal/debugger/compat/ottl/variant.go", "internal/debugger/compat/ottl/since0160/ottl.go", "internal/debugger/compat/ottl/since0161/ottl.go", "internal/debugger/compat/ottl/since0162/ottl.go",
		"internal/debugger/featureset/featureset.go", "internal/debugger/ottlreplay/replay.go", "internal/debugger/aggregation/count.go", "internal/debugger/routing/routing.go", "internal/debugger/sampling/tail.go",
		// The provider SDK as its own module, and the registry's providers.
		"collector/provider/go.mod", "collector/provider/provider.go", "internal/debugger/registry/providers.go",
		// The CLI, its platform and client, and a file only windows builds.
		"cmd/linea/main.go", "cmd/linea/version.go", "internal/debugger/platform/service.go", "internal/debugger/client/client.go", "internal/debugger/container/proc_other.go", "internal/debugger/workersrc/static/README.md"} {
		if _, err := fs.Stat(src, want); err != nil {
			t.Errorf("archive lacks %s", want)
		}
	}
	err = fs.WalkDir(src, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch {
		case strings.HasSuffix(p, "_test.go"):
			t.Errorf("archive holds a test file %s", p)
		case p != "go.mod" && p != "collector/provider/go.mod" && path.Base(p) == "go.mod", p != "go.sum" && path.Base(p) == "go.sum":
			t.Errorf("archive holds a nested module file %s", p)
		case strings.HasPrefix(p, "internal/debugger/server/"), strings.HasPrefix(p, "internal/debugger/webui/"), strings.HasPrefix(p, "internal/debugger/store/pg/"), strings.HasPrefix(p, "web/"), strings.HasPrefix(p, "cmd/linea-server/"):
			t.Errorf("archive holds %s, which neither the worker nor the CLI imports", p)
		case strings.HasSuffix(p, ".tar.gz"):
			t.Errorf("archive holds an archive %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestUnpackRefusesForeignArchives(t *testing.T) {
	if _, err := Unpack([]byte("not a gzip")); err == nil {
		t.Fatal("garbage accepted")
	}
}
