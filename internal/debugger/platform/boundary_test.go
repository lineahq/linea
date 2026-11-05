package platform

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const module = "github.com/lineahq/linea/"

// vocabulary packages name the platform's result documents. Adapters may refer
// to their types and constants to format results, never call their functions.
var vocabulary = []string{"internal/debugger/analysis", "internal/debugger/assertion"}

// TestAdaptersMapPlatformOperationsOnly enforces the import boundary (contract
// §5): adapters reach capabilities through platform operations, so each one is
// defined once. Test files are exempt.
func TestAdaptersMapPlatformOperationsOnly(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	adapters := map[string][]string{
		"internal/debugger/api": {"internal/debugger/platform", "samples"},
		"internal/debugger/mcp": {"internal/debugger/platform", "samples"},
		// cmd/linea inlines the files a scenario names before sending it to a
		// server that cannot read the client's disk (connected.go), and
		// installs the in-process Collector its direct commands run on.
		"cmd/linea": {"internal/debugger/platform", "internal/debugger/engine/local/install", "internal/debugger/artifact", "internal/debugger/store/memory", "internal/debugger/api", "internal/debugger/client", "internal/debugger/scenario", "samples"},
		// The team server composes the platform over its store with the HTTP
		// API, the web client and MCP.
		"internal/debugger/server": {"internal/debugger/platform", "internal/debugger/artifact", "internal/debugger/store/pg", "internal/debugger/api", "internal/debugger/mcp", "internal/debugger/webui"},
		"cmd/linea-server":         {"internal/debugger/server"},
	}
	for dir, allowed := range adapters {
		for _, f := range parseDir(t, filepath.Join(root, dir)) {
			names := map[string]string{} // Local name -> vocabulary package.
			for _, spec := range f.file.Imports {
				path, _ := strconv.Unquote(spec.Path.Value)
				if !strings.HasPrefix(path, module) {
					continue
				}
				pkg := strings.TrimPrefix(path, module)
				switch {
				case slices.Contains(allowed, pkg):
				case slices.Contains(vocabulary, pkg):
					name := filepath.Base(pkg)
					if spec.Name != nil {
						name = spec.Name.Name
					}
					names[name] = pkg
				default:
					t.Errorf("%s imports %s; adapters use platform operations", f.name, pkg)
				}
			}
			if len(names) == 0 {
				continue
			}
			ast.Inspect(f.file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				x, ok := sel.X.(*ast.Ident)
				if !ok || names[x.Name] == "" {
					return true
				}
				if !typeNames(t, filepath.Join(root, names[x.Name]))[sel.Sel.Name] {
					t.Errorf("%s calls %s.%s; vocabulary packages are for types and constants", f.name, x.Name, sel.Sel.Name)
				}
				return true
			})
		}
	}
}

// TestLowerLayersDoNotImportThePlatform keeps the dependency direction: the
// platform composes the engine and storage, never the reverse.
func TestLowerLayersDoNotImportThePlatform(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	for _, dir := range []string{"internal/debugger/engine", "internal/debugger/artifact", "internal/debugger/config", "internal/debugger/runtime", "internal/debugger/runresult", "internal/debugger/authz"} {
		for _, f := range parseDir(t, filepath.Join(root, dir)) {
			for _, spec := range f.file.Imports {
				path, _ := strconv.Unquote(spec.Path.Value)
				if path == module+"internal/debugger/platform" || path == module+"internal/debugger/api" {
					t.Errorf("%s imports %s", f.name, path)
				}
			}
		}
	}
}

type parsed struct {
	name string
	file *ast.File
}

// parseDir parses a package's non-test Go files.
func parseDir(t *testing.T, dir string) []parsed {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var files []parsed
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, parsed{name: filepath.ToSlash(path), file: f})
	}
	if len(files) == 0 {
		t.Fatalf("%s has no Go files", dir)
	}
	return files
}

var typeNameCache = map[string]map[string]bool{}

// typeNames returns a package's declared type names: a call through one is a
// conversion, not a function call.
func typeNames(t *testing.T, dir string) map[string]bool {
	if names, ok := typeNameCache[dir]; ok {
		return names
	}
	names := map[string]bool{}
	for _, f := range parseDir(t, dir) {
		for _, decl := range f.file.Decls {
			if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.TYPE {
				for _, spec := range gen.Specs {
					names[spec.(*ast.TypeSpec).Name.Name] = true
				}
			}
		}
	}
	typeNameCache[dir] = names
	return names
}
