package platform

import (
	"context"
	"errors"
	"go/ast"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/authz"
)

// declaredPermissions reads the permission each exported operation declares:
// the first authz permission its body names, or else the declaration of the
// first operation it delegates to.
func declaredPermissions(t *testing.T) map[string]authz.Permission {
	t.Helper()
	known := map[string]authz.Permission{}
	for _, p := range authz.Permissions() {
		known[string(p)] = p
	}
	constants := map[string]authz.Permission{
		"RunRead": authz.RunRead, "RunSubmit": authz.RunSubmit, "RunDelete": authz.RunDelete,
		"ArtifactRead": authz.ArtifactRead, "ArtifactWrite": authz.ArtifactWrite,
		"CaptureRead": authz.CaptureRead, "CaptureStart": authz.CaptureStart, "CaptureRaw": authz.CaptureRaw,
		"FleetRead": authz.FleetRead, "FleetConfigWrite": authz.FleetConfigWrite, "FleetRollout": authz.FleetRollout,
		"RuntimeRead": authz.RuntimeRead, "RuntimeManage": authz.RuntimeManage, "RuntimeBuild": authz.RuntimeBuild, "AuditRead": authz.AuditRead,
		"TokenOwn": authz.TokenOwn, "TokenManage": authz.TokenManage, "AccessManage": authz.AccessManage,
		"ProjectManage": authz.ProjectManage, "LibraryPublish": authz.LibraryPublish, "Admin": authz.Admin,
	}
	if len(constants) != len(known) {
		t.Fatalf("the test knows %d permissions, the catalog has %d", len(constants), len(known))
	}
	direct := map[string]authz.Permission{}
	delegates := map[string][]string{}
	for _, f := range parseDir(t, ".") {
		for _, d := range f.file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || !isOperation(fn) {
				continue
			}
			name := fn.Name.Name
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if x, ok := sel.X.(*ast.Ident); ok && x.Name == "authz" {
					if p, ok := constants[sel.Sel.Name]; ok && direct[name] == "" {
						direct[name] = p
					}
				}
				if x, ok := sel.X.(*ast.Ident); ok && x.Name == "s" && ast.IsExported(sel.Sel.Name) {
					delegates[name] = append(delegates[name], sel.Sel.Name)
				}
				return true
			})
			if _, ok := direct[name]; !ok {
				direct[name] = ""
			}
		}
	}
	out := map[string]authz.Permission{}
	var resolve func(string, int) authz.Permission
	resolve = func(name string, depth int) authz.Permission {
		if p := direct[name]; p != "" || depth > 4 {
			return p
		}
		for _, d := range delegates[name] {
			if _, op := direct[d]; op {
				if p := resolve(d, depth+1); p != "" {
					return p
				}
			}
		}
		return ""
	}
	for name := range direct {
		out[name] = resolve(name, 0)
	}
	return out
}

// isOperation reports whether fn is an exported *Service method taking a
// Caller, which is what makes it a platform operation.
func isOperation(fn *ast.FuncDecl) bool {
	if fn.Recv == nil || !fn.Name.IsExported() || len(fn.Recv.List) != 1 {
		return false
	}
	star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	if id, ok := star.X.(*ast.Ident); !ok || id.Name != "Service" {
		return false
	}
	// An operation can fail; Permissions, which lists the caller's own grants,
	// cannot and is not one.
	results := fn.Type.Results
	if results == nil || len(results.List) == 0 {
		return false
	}
	if id, ok := results.List[len(results.List)-1].Type.(*ast.Ident); !ok || id.Name != "error" {
		return false
	}
	for _, p := range fn.Type.Params.List {
		if id, ok := p.Type.(*ast.Ident); ok && id.Name == "Caller" {
			return true
		}
	}
	return false
}

// anyPermission lists the operations open to every caller who holds some
// permission anywhere, which refuse only a caller who holds none.
var anyPermission = map[string]bool{"ListProjects": true}

// TestEveryOperationDeclaresAPermission is the authorization coverage gate: an
// operation that names no permission fails here.
func TestEveryOperationDeclaresAPermission(t *testing.T) {
	declared := declaredPermissions(t)
	if len(declared) < 100 {
		t.Fatalf("found only %d operations", len(declared))
	}
	for name, p := range declared {
		if p == "" {
			t.Errorf("%s declares no permission: pass one to begin or an entry helper", name)
		}
	}
}

// TestUnboundPrincipalsAreRefusedEverywhere calls every operation as a
// principal without bindings and as a viewer: each refuses with forbidden,
// naming the permission the operation declares, before reading its request.
func TestUnboundPrincipalsAreRefusedEverywhere(t *testing.T) {
	declared := declaredPermissions(t)
	access, err := NewPolicy([]Binding{{Subject: Subject{Group: "readers"}, Role: "viewer", Scope: ScopeAll}})
	if err != nil {
		t.Fatal(err)
	}
	s := New(Options{Access: access})
	t.Cleanup(s.Close)
	viewer := authz.Identity{ID: "user:vera", Groups: []string{"readers"}}
	callers := map[string]Caller{
		"unbound": {Principal: Principal{ID: "user:mallory", Kind: PrincipalUser}, Client: "test"},
		"viewer":  {Principal: Principal{ID: viewer.ID, Kind: PrincipalUser, Groups: viewer.Groups}, Client: "test"},
	}
	v := reflect.ValueOf(s)
	callerType := reflect.TypeOf(Caller{})
	errType := reflect.TypeOf((*error)(nil)).Elem()
	called := 0
	for i := range v.NumMethod() {
		m := v.Type().Method(i)
		ft := m.Type
		if ft.NumIn() < 3 || ft.In(2) != callerType {
			continue
		}
		perm, ok := declared[m.Name]
		if !ok {
			t.Errorf("%s: no declaration found", m.Name)
			continue
		}
		called++
		for who, caller := range callers {
			if who == "viewer" && (anyPermission[m.Name] || access.Allowed(viewer, perm, authz.All)) {
				continue
			}
			args := []reflect.Value{reflect.ValueOf(context.Background()), reflect.ValueOf(caller)}
			for j := 3; j < ft.NumIn(); j++ {
				args = append(args, reflect.Zero(ft.In(j)))
			}
			out := v.Method(i).Call(args)
			last := out[len(out)-1]
			if last.Type() != errType {
				t.Fatalf("%s: last result is %s", m.Name, last.Type())
			}
			var e *Error
			if err, _ := last.Interface().(error); !errors.As(err, &e) || e.Code != CodeForbidden || !anyPermission[m.Name] && !strings.Contains(e.Message, string(perm)) {
				t.Errorf("%s as %s: %v, want forbidden naming %s", m.Name, who, last.Interface(), perm)
			}
		}
	}
	if called != len(declared) {
		t.Errorf("called %d operations, declared %d", called, len(declared))
	}
}

// TestLocalOwnerIsAdmin keeps local mode: the default policy grants the local
// owner every permission and nobody else anything.
func TestLocalOwnerIsAdmin(t *testing.T) {
	s := New(Options{})
	t.Cleanup(s.Close)
	for _, p := range authz.Permissions() {
		if err := s.authorize(Local("cli"), p); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	if err := s.authorize(Caller{Principal: Principal{ID: "user:alice", Kind: PrincipalUser}, Client: "http"}, authz.RunRead); err == nil {
		t.Error("local mode allows another principal")
	}
	if _, err := s.ListRuns(context.Background(), Caller{Principal: Principal{ID: "user:alice", Kind: PrincipalUser}, Client: "http"}, RunQuery{}); !slices.Contains([]Code{CodeForbidden}, codeOf(err)) {
		t.Errorf("list runs as another principal: %v", err)
	}
}

func codeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}
