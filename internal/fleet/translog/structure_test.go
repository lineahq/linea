package translog

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The transition log is the only way the fleet server writes about itself.
// This test makes that structural: it fails when any other logging call
// appears in internal/fleet or cmd/linea-fleet, so nothing can print an
// error string, a credential or a configuration around the typed helper.

// fleetRoots are the trees the rules cover, relative to the module root.
var fleetRoots = []string{"internal/fleet", "cmd/linea-fleet"}

const mainFile = "cmd/linea-fleet/main.go"

// enumTypes are the translog types a caller must never convert into.
var enumTypes = map[string]bool{"Event": true, "Reason": true, "State": true, "Listener": true, "IDKey": true, "CountKey": true, "DurationKey": true, "Attr": true}

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

// sources parses the non-test Go files of the fleet trees, outside translog.
func sources(t *testing.T) (map[string]*ast.File, *token.FileSet) {
	t.Helper()
	root := moduleRoot(t)
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	for _, tree := range fleetRoots {
		err := filepath.WalkDir(filepath.Join(root, filepath.FromSlash(tree)), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if d.IsDir() {
				if rel == "internal/fleet/translog" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			files[rel] = f
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(files) < 20 {
		t.Fatalf("parsed only %d files; the walk is broken", len(files))
	}
	return files, fset
}

func importName(spec *ast.ImportSpec) string {
	if spec.Name != nil {
		return spec.Name.Name
	}
	path, _ := strconv.Unquote(spec.Path.Value)
	return filepath.Base(path)
}

// pkgSelector returns x.Sel when e is a selector on the identifier pkg.
func pkgSelector(e ast.Expr, pkg string) (string, bool) {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok || id.Name != pkg {
		return "", false
	}
	return sel.Sel.Name, true
}

func TestNoLoggingOutsideTheTransitionLog(t *testing.T) {
	files, fset := sources(t)
	enclosing := func(f *ast.File, pos token.Pos) string {
		for _, d := range f.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Pos() <= pos && pos <= fn.End() {
				return fn.Name.Name
			}
		}
		return ""
	}
	var allowedStderr int
	for name, f := range files {
		for _, spec := range f.Imports {
			switch path, _ := strconv.Unquote(spec.Path.Value); path {
			case "log", "log/slog", "log/syslog":
				t.Errorf("%s imports %s; log through translog", name, path)
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if n == nil {
				return false
			}
			where := fset.Position(n.Pos()).String()
			switch n := n.(type) {
			case *ast.SelectorExpr:
				// os.Stderr and os.Stdout may be named only by main, which
				// hands them to run and prints the one final error line.
				if member, ok := pkgSelector(n, "os"); ok && (member == "Stderr" || member == "Stdout") {
					if name != mainFile || enclosing(f, n.Pos()) != "main" {
						t.Errorf("%s: os.%s outside func main of %s", where, member, mainFile)
					}
				}
			case *ast.CallExpr:
				if fn, ok := n.Fun.(*ast.Ident); ok && (fn.Name == "print" || fn.Name == "println") {
					t.Errorf("%s: builtin %s", where, fn.Name)
				}
				if member, ok := pkgSelector(n.Fun, "fmt"); ok && strings.HasPrefix(member, "Print") {
					t.Errorf("%s: fmt.%s writes to standard output", where, member)
				}
				if member, ok := pkgSelector(n.Fun, "fmt"); ok && strings.HasPrefix(member, "Fprint") && len(n.Args) > 0 {
					switch dst := n.Args[0].(type) {
					case *ast.SelectorExpr:
						if _, isOS := pkgSelector(dst, "os"); isOS {
							if name == mainFile && enclosing(f, n.Pos()) == "main" && dst.Sel.Name == "Stderr" {
								allowedStderr++
							} else {
								t.Errorf("%s: fmt.%s to os.%s", where, member, dst.Sel.Name)
							}
						}
					case *ast.Ident:
						// Command results go to the stdout parameter of the
						// CLI; nothing may be written to a stderr parameter.
						if dst.Name == "stderr" || (dst.Name == "stdout" && name != mainFile) {
							t.Errorf("%s: fmt.%s to %s", where, member, dst.Name)
						}
					}
				}
				// A conversion such as translog.Reason(x) would let a value
				// that is not one of the constants into the log.
				if typ, ok := pkgSelector(n.Fun, "translog"); ok && enumTypes[typ] {
					t.Errorf("%s: conversion to translog.%s; use the constants", where, typ)
				}
			case *ast.CompositeLit:
				if typ, ok := pkgSelector(n.Type, "translog"); ok && enumTypes[typ] {
					t.Errorf("%s: translog.%s literal", where, typ)
				}
			}
			return true
		})
	}
	if allowedStderr != 1 {
		t.Errorf("%s must write the final error line exactly once, found %d writes to os.Stderr", mainFile, allowedStderr)
	}
}

// TestLogCallsTakeOnlyTypedArguments checks the arguments of every call on a
// logger: the event is a translog constant, every field is built by a
// translog constructor, and constructors get a translog key constant and no
// literal for an enumeration.
func TestLogCallsTakeOnlyTypedArguments(t *testing.T) {
	files, fset := sources(t)
	isLogger := func(e ast.Expr) bool {
		switch e := e.(type) {
		case *ast.Ident:
			return e.Name == "log"
		case *ast.SelectorExpr:
			return e.Sel.Name == "log"
		}
		return false
	}
	var calls, constructors int
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			where := fset.Position(call.Pos()).String()
			if ctor, ok := pkgSelector(call.Fun, "translog"); ok {
				constructors++
				switch ctor {
				case "ID", "Count", "Duration":
					if _, ok := pkgSelector(call.Args[0], "translog"); !ok {
						t.Errorf("%s: translog.%s needs a translog key constant", where, ctor)
					}
				case "Why", "Now", "On", "Of":
					if _, isLit := call.Args[0].(*ast.BasicLit); isLit {
						t.Errorf("%s: translog.%s with a literal", where, ctor)
					}
				}
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !isLogger(sel.X) {
				return true
			}
			switch sel.Sel.Name {
			case "Info", "Warn", "Error", "Limited":
			case "AuthFailure":
				calls++
				if _, ok := pkgSelector(call.Args[0], "translog"); !ok {
					t.Errorf("%s: AuthFailure needs a translog listener constant", where)
				}
				return true
			default:
				return true
			}
			calls++
			if _, ok := pkgSelector(call.Args[0], "translog"); !ok {
				t.Errorf("%s: %s needs a translog event constant", where, sel.Sel.Name)
			}
			for i, arg := range call.Args[1:] {
				if c, ok := arg.(*ast.CallExpr); ok {
					if _, ok := pkgSelector(c.Fun, "translog"); ok {
						continue
					}
				}
				// The only other form is the spread of a prepared list.
				if _, ok := arg.(*ast.Ident); ok && call.Ellipsis.IsValid() && i == len(call.Args)-2 {
					continue
				}
				t.Errorf("%s: field %d is not built by a translog constructor", where, i+1)
			}
			return true
		})
	}
	if calls < 20 || constructors < 20 {
		t.Fatalf("found %d log calls and %d constructors; the wiring is missing or the test is broken", calls, constructors)
	}
}
