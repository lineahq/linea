package container

import (
	"errors"
	"go/parser"
	"go/token"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/lineahq/linea/internal/debugger/release"
)

func cliSource(t *testing.T) fstest.MapFS {
	t.Helper()
	src := repoSource(t)
	src["cmd/linea/main.go"] = &fstest.MapFile{Data: []byte("package main\n")}
	return src
}

// TestCLIStampIsGoThatNamesTheBuild checks the generated stamp: valid Go,
// the support label, and each source without a git URL's credentials.
func TestCLIStampIsGoThatNamesTheBuild(t *testing.T) {
	tests := []struct {
		name     string
		verified bool
		sources  []ModuleSource
		want     []string
		refuse   []string
	}{
		{name: "verified, no sources", verified: true, want: []string{"Verified: true"}, refuse: []string{"Modules"}},
		{
			name: "unverified with sources",
			sources: []ModuleSource{
				{Module: "example.com/a", Origin: "/src/a"},
				{Module: "example.com/b", Origin: "https://user:secret@git.example.com/b.git@v1#sub", Commit: "abc123"},
			},
			want:   []string{"Verified: false", `"example.com/a"`, `"/src/a"`, `"https://git.example.com/b.git@v1#sub"`, `"abc123"`},
			refuse: []string{"secret", "user"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := cliStamp(&buildPlan{verified: tt.verified}, tt.sources)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := parser.ParseFile(token.NewFileSet(), cliStampFile, out, 0); err != nil {
				t.Fatalf("the stamp is not Go: %v\n%s", err, out)
			}
			for _, w := range tt.want {
				if !strings.Contains(string(out), w) {
					t.Errorf("the stamp lacks %s:\n%s", w, out)
				}
			}
			for _, r := range tt.refuse {
				if strings.Contains(string(out), r) {
					t.Errorf("the stamp holds %s:\n%s", r, out)
				}
			}
		})
	}
}

// TestCLIPlanWritesTheStampIntoTheArchive plans the built-in manifest as a
// CLI build: the archive holds the stamp beside the CLI's main package.
func TestCLIPlanWritesTheStampIntoTheArchive(t *testing.T) {
	src := cliSource(t)
	_, p, _, err := cliPlan(CLIRequest{Manifest: builtInManifest(t), Source: src, Set: set162(t)})
	if err != nil {
		t.Fatal(err)
	}
	archive, err := p.writeArchive(src, p.goMod, p.goSum, nil)
	if err != nil {
		t.Fatal(err)
	}
	files := untar(t, archive)
	if !strings.Contains(files[cliStampFile], "customBuild = &buildStamp{Verified: true}") {
		t.Fatalf("the archive's %s is %q", cliStampFile, files[cliStampFile])
	}
	if files["cmd/linea/main.go"] == "" {
		t.Fatal("the archive lacks the CLI's main package")
	}
}

// TestCLIPlanRefusesASourceWithoutTheCLI: a worker source from before the
// CLI was embedded cannot build one.
func TestCLIPlanRefusesASourceWithoutTheCLI(t *testing.T) {
	_, _, _, err := cliPlan(CLIRequest{Manifest: builtInManifest(t), Source: repoSource(t), Set: set162(t)})
	if err == nil || !strings.Contains(err.Error(), "holds no CLI") {
		t.Fatalf("got %v, want the missing CLI", err)
	}
}

// TestWriteArchiveRefusesAnExtraFileTheSourceHas: the stamp never
// silently replaces a file of the source.
func TestWriteArchiveRefusesAnExtraFileTheSourceHas(t *testing.T) {
	src := cliSource(t)
	src[cliStampFile] = &fstest.MapFile{Data: []byte("package main\n")}
	_, p, _, err := cliPlan(CLIRequest{Manifest: builtInManifest(t), Source: src, Set: set162(t)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.writeArchive(src, p.goMod, p.goSum, nil); err == nil {
		t.Fatal("an archive replaced the source's own stamp file")
	}
}

// TestCLICompileArgsBuildTheCLI: the CLI main, the product version, no
// worker stamps, and the unverified mark only for an unverified release.
func TestCLICompileArgsBuildTheCLI(t *testing.T) {
	for _, verified := range []bool{true, false} {
		args := cliCompileArgs("/tmp/linea", verified)
		if args[len(args)-1] != cliMain || !slices.Contains(args, "/tmp/linea") {
			t.Fatalf("args %v do not build %s into /tmp/linea", args, cliMain)
		}
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, release.VersionVariable+"="+release.ProductVersion) {
			t.Fatalf("args %v do not stamp the product version", args)
		}
		if strings.Contains(joined, "worker.") {
			t.Fatalf("args %v stamp worker variables into the CLI", args)
		}
		if marked := strings.Contains(joined, release.UnverifiedVariable+"=true"); marked == verified {
			t.Fatalf("verified %t: args %v", verified, args)
		}
	}
}

// TestCLIErrorNamesTheCLI: a CLI build's BuildError says CLI, a runtime
// build's still says runtime.
func TestCLIErrorNamesTheCLI(t *testing.T) {
	be := &BuildError{Stage: "compiling", Err: errors.New("boom")}
	if !strings.HasPrefix(be.Error(), "runtime build failed") {
		t.Fatalf("runtime build error: %s", be)
	}
	if err := cliError(be); !strings.HasPrefix(err.Error(), "CLI build failed while compiling") {
		t.Fatalf("CLI build error: %s", err)
	}
	if cliError(nil) != nil {
		t.Fatal("cliError invented an error")
	}
}
