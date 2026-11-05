package manifest

import (
	"go/ast"
	"go/token"
	"io/fs"

	"golang.org/x/mod/modfile"
)

// ProviderSDK is the provider SDK's module path.
const ProviderSDK = "github.com/lineahq/linea/collector/provider"

// Provider is a debugger provider found in a module's source: a package
// exporting func DebuggerProvider() provider.Provider.
type Provider struct {
	// Import is the package's import path.
	Import string
	// Name is the provider's Name when the source states it as a string
	// literal or constant in the returned composite literal; empty when
	// unknown, which the worker resolves when it starts.
	Name string
}

// DiscoverProviders lists the debugger providers in a module's source,
// read as Discover reads components, and the SDK version the module's
// go.mod requires ("" when it requires none).
func DiscoverProviders(files fs.FS) (module string, providers []Provider, sdk string, err error) {
	src, err := parseModule(files)
	if err != nil {
		return "", nil, "", err
	}
	if mf, err := modfile.ParseLax("go.mod", src.goMod, nil); err == nil {
		for _, r := range mf.Require {
			if r.Mod.Path == ProviderSDK {
				sdk = r.Mod.Version
			}
		}
	}
	types := typeResolver{module: src.module, pkgs: src.pkgs, consts: map[string]map[string]string{}}
	for _, dir := range src.importable() {
		for _, f := range src.pkgs[dir] {
			if fn, ok := debuggerProvider(f); ok {
				providers = append(providers, Provider{Import: src.importPath(dir), Name: providerName(&types, dir, f, fn)})
				break
			}
		}
	}
	return src.module, providers, sdk, nil
}

// debuggerProvider finds f's func DebuggerProvider() <sdk>.Provider.
func debuggerProvider(f *ast.File) (*ast.FuncDecl, bool) {
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || fn.Name.Name != "DebuggerProvider" || fn.Type.Params.NumFields() != 0 ||
			fn.Type.Results.NumFields() != 1 || fn.Type.TypeParams != nil || fn.Body == nil {
			continue
		}
		if isSDKProvider(f, fn.Type.Results.List[0].Type) {
			return fn, true
		}
	}
	return nil, false
}

func isSDKProvider(f *ast.File, e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Provider" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	p, ok := importOf(f, pkg.Name)
	return ok && p == ProviderSDK
}

// providerName reads the Name field of the first SDK Provider composite
// literal in fn.
func providerName(r *typeResolver, dir string, f *ast.File, fn *ast.FuncDecl) string {
	name := ""
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok || name != "" {
			return name == ""
		}
		if !isSDKProvider(f, lit.Type) {
			return true
		}
		for _, el := range lit.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Name" {
				if b, ok := kv.Value.(*ast.BasicLit); ok && b.Kind != token.STRING {
					return false
				}
				name = r.value(dir, f, kv.Value)
			}
		}
		return false
	})
	return name
}
