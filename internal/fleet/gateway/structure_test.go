package gateway

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// commandFields are the ServerToAgent fields that make an agent change: run
// a command, install packages, switch connections or load a config. Fleet
// reporting is read-only toward agents, so only the opt-in config management
// path in gateway/config.go may set them.
var commandFields = map[string]bool{"Command": true, "PackagesAvailable": true, "ConnectionSettings": true, "RemoteConfig": true}

const commandFile = "internal/fleet/gateway/config.go"

// TestOnlyConfigFileSendsAgentCommands fails when fleet code outside
// gateway/config.go sets a command field of an OpAMP ServerToAgent, in a
// composite literal or by assigning to a variable or parameter declared as
// one.
func TestOnlyConfigFileSendsAgentCommands(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()
	allowed := 0
	for _, tree := range []string{"internal/fleet", "cmd/linea-fleet", "cmd/linea-fleet-simulator"} {
		err := filepath.WalkDir(filepath.Join(root, filepath.FromSlash(tree)), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			for _, pos := range commandSets(f) {
				if rel == commandFile {
					allowed++
					continue
				}
				t.Errorf("%s: sets an agent command field outside %s", fset.Position(pos), commandFile)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// The config path itself must be seen, or the check finds nothing.
	if allowed < 2 {
		t.Fatalf("found %d command field sets in %s, want the push and the reply", allowed, commandFile)
	}
}

// isServerToAgent reports whether e names protobufs.ServerToAgent or a
// pointer to it.
func isServerToAgent(e ast.Expr) bool {
	if star, ok := e.(*ast.StarExpr); ok {
		e = star.X
	}
	if u, ok := e.(*ast.UnaryExpr); ok && u.Op == token.AND {
		e = u.X
	}
	if lit, ok := e.(*ast.CompositeLit); ok {
		e = lit.Type
	}
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "ServerToAgent" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "protobufs"
}

// commandSets returns where f sets a command field: as a key of a
// ServerToAgent literal, or as x.Field = ... where x is declared in the same
// function as a ServerToAgent (literal, var or parameter).
func commandSets(f *ast.File) []token.Pos {
	var out []token.Pos
	ast.Inspect(f, func(n ast.Node) bool {
		if lit, ok := n.(*ast.CompositeLit); ok && isServerToAgent(lit.Type) {
			for _, e := range lit.Elts {
				if kv, ok := e.(*ast.KeyValueExpr); ok {
					if key, ok := kv.Key.(*ast.Ident); ok && commandFields[key.Name] {
						out = append(out, kv.Pos())
					}
				}
			}
		}
		return true
	})
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		names := map[string]bool{}
		for _, field := range fn.Type.Params.List {
			if isServerToAgent(field.Type) {
				for _, name := range field.Names {
					names[name.Name] = true
				}
			}
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch s := n.(type) {
			case *ast.ValueSpec:
				if s.Type != nil && isServerToAgent(s.Type) {
					for _, name := range s.Names {
						names[name.Name] = true
					}
				}
				for i, v := range s.Values {
					if isServerToAgent(v) && i < len(s.Names) {
						names[s.Names[i].Name] = true
					}
				}
			case *ast.AssignStmt:
				for i, rhs := range s.Rhs {
					if id, ok := s.Lhs[min(i, len(s.Lhs)-1)].(*ast.Ident); ok && isServerToAgent(rhs) {
						names[id.Name] = true
					}
				}
			}
			return true
		})
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			s, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for _, lhs := range s.Lhs {
				sel, ok := lhs.(*ast.SelectorExpr)
				if !ok || !commandFields[sel.Sel.Name] {
					continue
				}
				if id, ok := sel.X.(*ast.Ident); ok && names[id.Name] {
					out = append(out, sel.Pos())
				}
			}
			return true
		})
	}
	return out
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if data, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil && strings.Contains(string(data), "module github.com/lineahq/linea\n") {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("module root not found")
		}
		dir = parent
	}
}
