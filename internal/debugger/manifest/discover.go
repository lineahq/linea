package manifest

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/mod/modfile"
)

// SourceVersion is the version a manifest entry gives a module whose
// source is supplied with the build: the source replaces it, so any valid
// version does, and OCB manifests conventionally write this one.
const SourceVersion = "v0.0.0"

// Component is a component package found in a module's source.
type Component struct {
	Kind Kind
	// Import is the package's import path.
	Import string
	// Type is the component type NewFactory passes to the Collector's
	// NewFactory, when the source states it as a string: a literal or a
	// constant given to component.MustNewType, directly or through a
	// package variable (mdatagen's metadata.Type). Empty when unknown.
	Type string
}

// Entry returns the manifest entry that builds c from module's supplied
// source.
func (c Component) Entry(module string) Module {
	m := Module{GoMod: module + " " + SourceVersion}
	if c.Import != module {
		m.Import = c.Import
	}
	return m
}

// collectorKinds maps the Collector packages whose Factory a component's
// NewFactory returns to the component's kind; the x<kind> packages hold
// the experimental factories.
var collectorKinds = map[string]Kind{
	"receiver": Receivers, "processor": Processors, "exporter": Exporters,
	"connector": Connectors, "extension": Extensions,
}

func factoryKind(importPath string) (Kind, bool) {
	rest, ok := strings.CutPrefix(importPath, "go.opentelemetry.io/collector/")
	if !ok {
		return "", false
	}
	base, sub, _ := strings.Cut(rest, "/")
	kind, ok := collectorKinds[base]
	if !ok || sub != "" && sub != "x"+base {
		return "", false
	}
	return kind, true
}

// Discover lists the component packages of a module's source, read from
// files (the module's directory, holding go.mod): the importable packages
// with a function NewFactory() returning a Collector receiver, processor,
// exporter, connector or extension Factory, as OCB imports them. Test
// files, main packages and the directories testdata, vendor, internal,
// those starting with '.' or '_' and nested modules are skipped. It returns
// the module path go.mod declares. Each component's Type is read from its
// NewFactory where the source states it.
func Discover(files fs.FS) (string, []Component, error) {
	src, err := parseModule(files)
	if err != nil {
		return "", nil, err
	}
	types := typeResolver{module: src.module, pkgs: src.pkgs, consts: map[string]map[string]string{}}
	var out []Component
	for _, dir := range src.importable() {
		for _, f := range src.pkgs[dir] {
			if kind, fn, ok := newFactoryKind(f); ok {
				out = append(out, Component{Kind: kind, Import: src.importPath(dir), Type: types.factoryType(dir, f, fn)})
				break
			}
		}
	}
	slices.SortFunc(out, func(a, b Component) int {
		if c := slices.Index(Kinds, a.Kind) - slices.Index(Kinds, b.Kind); c != 0 {
			return c
		}
		return strings.Compare(a.Import, b.Import)
	})
	return src.module, out, nil
}

// moduleSource is a module's parsed source: its non-main packages by
// directory, internal ones too, skipping what Discover skips.
type moduleSource struct {
	module string
	goMod  []byte
	pkgs   map[string][]*ast.File
}

func parseModule(files fs.FS) (*moduleSource, error) {
	goMod, err := fs.ReadFile(files, "go.mod")
	if err != nil {
		return nil, fmt.Errorf("the source has no go.mod")
	}
	module := modfile.ModulePath(goMod)
	if module == "" {
		return nil, fmt.Errorf("go.mod declares no module")
	}
	out := &moduleSource{module: module, goMod: goMod, pkgs: map[string][]*ast.File{}}
	fset := token.NewFileSet()
	err = fs.WalkDir(files, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name == "." {
				return nil
			}
			base := d.Name()
			if base == "testdata" || base == "vendor" || strings.HasPrefix(base, ".") || strings.HasPrefix(base, "_") {
				return fs.SkipDir
			}
			if _, err := fs.Stat(files, path.Join(name, "go.mod")); err == nil {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		src, err := fs.ReadFile(files, name)
		if err != nil {
			return err
		}
		f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
		if err != nil || f.Name.Name == "main" {
			return nil // The build reports what does not compile.
		}
		dir := path.Dir(name)
		out.pkgs[dir] = append(out.pkgs[dir], f)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// importable lists the directories of packages a build can import, sorted.
func (m *moduleSource) importable() []string {
	var out []string
	for dir := range m.pkgs {
		if !slices.Contains(strings.Split(dir, "/"), "internal") {
			out = append(out, dir)
		}
	}
	slices.Sort(out)
	return out
}

func (m *moduleSource) importPath(dir string) string {
	if dir == "." {
		return m.module
	}
	return m.module + "/" + dir
}

// newFactoryKind reports the kind of the factory a file's NewFactory()
// returns, and the function.
func newFactoryKind(f *ast.File) (Kind, *ast.FuncDecl, bool) {
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || fn.Name.Name != "NewFactory" || fn.Type.Params.NumFields() != 0 ||
			fn.Type.Results.NumFields() != 1 || fn.Type.TypeParams != nil {
			continue
		}
		sel, ok := fn.Type.Results.List[0].Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Factory" {
			continue
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok {
			continue
		}
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			name := path.Base(p)
			if imp.Name != nil {
				name = imp.Name.Name
			}
			if name == pkg.Name {
				kind, ok := factoryKind(p)
				return kind, fn, ok
			}
		}
	}
	return "", nil, false
}

// typeResolver reads component types from a module's parsed packages.
type typeResolver struct {
	module string
	pkgs   map[string][]*ast.File
	// consts caches each directory's package-level string values: string
	// constants and variables set to component.MustNewType of one.
	consts map[string]map[string]string
}

// factoryType is the type fn passes as the first argument of a Collector
// <kind>.NewFactory call, or "".
func (r *typeResolver) factoryType(dir string, f *ast.File, fn *ast.FuncDecl) string {
	typ := ""
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || typ != "" || len(call.Args) == 0 {
			return typ == ""
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "NewFactory" {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if p, ok := importOf(f, pkg.Name); ok {
			if _, ok := factoryKind(p); ok {
				typ = r.value(dir, f, call.Args[0])
			}
		}
		return typ == ""
	})
	return typ
}

// value evaluates e, in dir's file f, to a type string: a string literal,
// component.MustNewType of one, or a package-level constant or variable
// holding either, in this package or another of the module's.
func (r *typeResolver) value(dir string, f *ast.File, e ast.Expr) string {
	switch e := e.(type) {
	case *ast.BasicLit:
		if e.Kind == token.STRING {
			if s, err := strconv.Unquote(e.Value); err == nil {
				return s
			}
		}
	case *ast.Ident:
		return r.names(dir)[e.Name]
	case *ast.SelectorExpr:
		pkg, ok := e.X.(*ast.Ident)
		if !ok {
			return ""
		}
		p, ok := importOf(f, pkg.Name)
		if !ok {
			return ""
		}
		other, ok := strings.CutPrefix(p, r.module+"/")
		if !ok {
			return ""
		}
		return r.names(other)[e.Sel.Name]
	case *ast.CallExpr:
		sel, ok := e.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "MustNewType" || len(e.Args) != 1 {
			return ""
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok {
			return ""
		} else if p, ok := importOf(f, pkg.Name); !ok || p != "go.opentelemetry.io/collector/component" {
			return ""
		}
		return r.value(dir, f, e.Args[0])
	}
	return ""
}

// names returns dir's package-level string values, read once.
func (r *typeResolver) names(dir string) map[string]string {
	if v, ok := r.consts[dir]; ok {
		return v
	}
	v := map[string]string{}
	r.consts[dir] = v // Guards against cycles while it fills.
	for _, f := range r.pkgs[dir] {
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST && gen.Tok != token.VAR {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || len(vs.Names) != len(vs.Values) {
					continue
				}
				for i, name := range vs.Names {
					if s := r.value(dir, f, vs.Values[i]); s != "" {
						v[name.Name] = s
					}
				}
			}
		}
	}
	return v
}

// importOf returns the import path f binds to name.
func importOf(f *ast.File, name string) (string, bool) {
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		bound := path.Base(p)
		if imp.Name != nil {
			bound = imp.Name.Name
		}
		if bound == name {
			return p, true
		}
	}
	return "", false
}
