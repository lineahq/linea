package translog

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Where the plugin's logging is audited: the plugin package tree (this
// package excepted, it is the one place that may call log/slog) and its main.
var auditedRoots = []string{"..", "../../../../cmd/linea-fleet-livecapture"}

type auditedFile struct {
	path string
	fset *token.FileSet
	file *ast.File
}

func parseSource(t *testing.T, path string, src any) auditedFile {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	return auditedFile{path, fset, file}
}

func auditedFiles(t *testing.T) []auditedFile {
	t.Helper()
	var files []auditedFile
	for _, root := range auditedRoots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if filepath.Base(path) == "translog" || filepath.Base(path) == "migrations" {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
				files = append(files, parseSource(t, path, nil))
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(files) < 15 {
		t.Fatalf("audit found only %d files; the roots moved", len(files))
	}
	return files
}

// pkgSelector reports whether e is pkg.name and returns name.
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

func asExpr(n ast.Node) ast.Expr {
	e, _ := n.(ast.Expr)
	return e
}

// loggingViolations lists the logging calls outside the helper: a log or slog
// import, a print builtin, os.Stderr used anywhere but as the writer handed to
// translog.New.
func loggingViolations(f auditedFile) []string {
	var out []string
	for _, imp := range f.file.Imports {
		switch path := strings.Trim(imp.Path.Value, `"`); path {
		case "log", "log/slog", "github.com/rs/zerolog", "go.uber.org/zap", "github.com/sirupsen/logrus":
			out = append(out, fmt.Sprintf("%s imports %s", f.path, path))
		}
	}
	allowed := map[ast.Node]bool{}
	ast.Inspect(f.file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if name, ok := pkgSelector(call.Fun, "translog"); ok && name == "New" {
			for _, arg := range call.Args {
				allowed[arg] = true
			}
		}
		if id, ok := call.Fun.(*ast.Ident); ok && (id.Name == "print" || id.Name == "println") {
			out = append(out, fmt.Sprintf("%s: builtin %s", f.fset.Position(call.Pos()), id.Name))
		}
		return true
	})
	ast.Inspect(f.file, func(n ast.Node) bool {
		if name, ok := pkgSelector(asExpr(n), "os"); ok && name == "Stderr" && !allowed[n] {
			out = append(out, fmt.Sprintf("%s: os.Stderr used outside translog.New", f.fset.Position(n.Pos())))
		}
		return true
	})
	return out
}

// enumLookup reports whether call is a translog.Parse* lookup or CauseOf,
// which answer from closed tables and never from text.
func enumLookup(call *ast.CallExpr) bool {
	if name, ok := pkgSelector(call.Fun, "translog"); ok {
		return strings.HasPrefix(name, "Parse")
	}
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name == "CauseOf"
	case *ast.SelectorExpr:
		return fn.Sel.Name == "CauseOf"
	}
	return false
}

// callViolations lists the log calls that name their event or attributes
// otherwise than by constant or typed constructor, and the conversions into
// the helper's types; it also returns how many log calls it saw.
func callViolations(f auditedFile) (out []string, calls int) {
	enumTypes := []string{"Event", "Reason", "State", "Link", "Listener", "Attr"}
	enumArgs := []string{"Why", "Cause", "From", "To", "LinkState", "OnListener"}
	ast.Inspect(f.file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		where := f.fset.Position(call.Pos())
		if name, ok := pkgSelector(call.Fun, "translog"); ok {
			if slices.Contains(enumTypes, name) {
				out = append(out, fmt.Sprintf("%s: conversion to translog.%s", where, name))
			}
			if slices.Contains(enumArgs, name) && len(call.Args) == 1 {
				switch arg := call.Args[0].(type) {
				case *ast.Ident: // a typed enum variable: only the helper's constants and lookups make one
				case *ast.SelectorExpr:
					if _, ok := pkgSelector(arg, "translog"); !ok {
						out = append(out, fmt.Sprintf("%s: translog.%s argument is not a translog constant", where, name))
					}
				case *ast.CallExpr:
					if !enumLookup(arg) {
						out = append(out, fmt.Sprintf("%s: translog.%s argument is not a closed lookup", where, name))
					}
				default:
					out = append(out, fmt.Sprintf("%s: translog.%s argument is not a constant, enum value or lookup", where, name))
				}
			}
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Log" || len(call.Args) == 0 {
			return true
		}
		calls++
		if name, ok := pkgSelector(call.Args[0], "translog"); !ok || !strings.HasPrefix(name, "Event") {
			out = append(out, fmt.Sprintf("%s: Log without a translog.Event constant", where))
		}
		for _, arg := range call.Args[1:] {
			attr, ok := arg.(*ast.CallExpr)
			if !ok {
				out = append(out, fmt.Sprintf("%s: Log attribute is not a translog constructor call", where))
				continue
			}
			if _, ok := pkgSelector(attr.Fun, "translog"); !ok {
				out = append(out, fmt.Sprintf("%s: Log attribute is not a translog constructor call", where))
			}
		}
		return true
	})
	return out, calls
}

// No logging call exists outside the helper: no log or slog import, no print
// builtin, no write to os.Stderr except as the writer handed to translog.New.
func TestNoOtherLoggingCallExists(t *testing.T) {
	for _, f := range auditedFiles(t) {
		for _, v := range loggingViolations(f) {
			t.Error(v)
		}
	}
}

// Every log call names its event and its attributes by constant or by a typed
// constructor, and nothing outside the helper converts into its enum types.
func TestLogCallsUseConstantsAndConstructors(t *testing.T) {
	total := 0
	for _, f := range auditedFiles(t) {
		violations, calls := callViolations(f)
		total += calls
		for _, v := range violations {
			t.Error(v)
		}
	}
	if total < 15 {
		t.Fatalf("audit saw only %d log calls; the matcher is not looking at the code", total)
	}
}

// The audit must fail on the mistakes it exists to catch.
func TestAuditDetectsViolations(t *testing.T) {
	logging := map[string]string{
		"import log":      "package p\nimport \"log\"\nfunc f() { log.Println(\"x\") }",
		"import slog":     "package p\nimport \"log/slog\"\nfunc f() { slog.Info(\"x\") }",
		"stderr write":    "package p\nimport (\"fmt\"; \"os\")\nfunc f(err error) { fmt.Fprintln(os.Stderr, err) }",
		"println builtin": "package p\nfunc f() { println(\"x\") }",
	}
	for name, src := range logging {
		if len(loggingViolations(parseSource(t, "x.go", src))) == 0 {
			t.Errorf("%s was not detected", name)
		}
	}
	if len(loggingViolations(parseSource(t, "x.go", "package p\nimport (\"os\"; \"translog\")\nvar l = translog.New(os.Stderr)"))) != 0 {
		t.Error("translog.New(os.Stderr) was refused")
	}
	calls := map[string]string{
		"event from a variable": "package p\nfunc f(l L, e translog.Event) { l.Log(e) }",
		"reason conversion":     "package p\nfunc f(l L, s string) { l.Log(translog.EventPluginStopped, translog.Why(translog.Reason(s))) }",
		"raw attribute":         "package p\nfunc f(l L, a translog.Attr) { l.Log(translog.EventPluginStopped, a) }",
		"literal reason":        "package p\nfunc f(l L) { l.Log(translog.EventPluginStopped, translog.Cause(\"x\")) }",
		"lookup of other call":  "package p\nfunc f(l L, s string) { l.Log(translog.EventPluginStopped, translog.Cause(reasonOf(s))) }",
	}
	for name, src := range calls {
		if v, _ := callViolations(parseSource(t, "x.go", src)); len(v) == 0 {
			t.Errorf("%s was not detected", name)
		}
	}
	good := "package p\nfunc f(l L, s string, err error) { l.Log(translog.EventPluginStopped, translog.Why(translog.ParseReason(s)), translog.Cause(CauseOf(err)), translog.Cause(translog.ReasonShutdown)) }"
	if v, n := callViolations(parseSource(t, "x.go", good)); len(v) != 0 || n != 1 {
		t.Errorf("valid calls refused: %v (%d calls)", v, n)
	}
}

// The helper's exported API takes no error, free message, map or interface.
// A string parameter is allowed only where the function checks it against a
// character set or a closed table; Text reads a field and Write is the
// recorder's io.Writer.
func TestHelperAPITakesNoFreeText(t *testing.T) {
	stringOK := []string{"Operation", "Peer", "Credential", "Agent", "Authority", "Instance", "Epoch", "Version", "ParseReason", "ParseState", "ParseLink", "Text"}
	typeOK := []string{"Event", "Reason", "State", "Link", "Listener", "Attr", "...Attr", "int", "time.Duration", "time.Time", "io.Writer", "*Logger"}
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || !fn.Name.IsExported() {
					continue
				}
				if fn.Recv != nil && !ast.IsExported(strings.TrimPrefix(types.ExprString(fn.Recv.List[0].Type), "*")) {
					continue
				}
				checked++
				for _, field := range fn.Type.Params.List {
					typ := types.ExprString(field.Type)
					switch {
					case typ == "string" && slices.Contains(stringOK, fn.Name.Name):
					case typ == "[]byte" && fn.Name.Name == "Write":
					case slices.Contains(typeOK, typ):
					default:
						t.Errorf("%s takes a %s", fn.Name.Name, typ)
					}
				}
			}
		}
	}
	if checked < 30 {
		t.Fatalf("checked only %d exported functions", checked)
	}
}
