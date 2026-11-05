package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/platform"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

// adminAuthenticator signs every request in as a member of group admins.
type adminAuthenticator struct{}

func (adminAuthenticator) Authenticate(*http.Request) (platform.Principal, error) {
	return platform.Principal{ID: "user:idp/root", Kind: platform.PrincipalUser, Groups: []string{"admins"}}, nil
}

// TestAccessGrantsListsAndRevokes drives linea access against a server: grant
// reads the current revision, list shows both sources, revoke removes the
// managed binding, and a server config binding cannot be revoked.
func TestAccessGrantsListsAndRevokes(t *testing.T) {
	access, err := platform.NewPolicy([]platform.Binding{{Subject: platform.Subject{Group: "admins"}, Role: "admin", Scope: platform.ScopeAll}})
	if err != nil {
		t.Fatal(err)
	}
	service, err := platform.Open(memory.New(artifact.Limits{}), platform.Options{Access: access})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	h := api.New(api.Options{Platform: service, RemoteAccess: true, Authenticator: adminAuthenticator{}})
	t.Cleanup(h.Close)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	linea := func(want int, args ...string) string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), append(args, "--server", srv.URL), &stdout, &stderr); code != want {
			t.Fatalf("%v: exit %d, want %d: %s", args, code, want, stderr.String())
		}
		return stdout.String() + stderr.String()
	}

	if out := linea(exitError, "access", "grant", "--role", "viewer"); !strings.Contains(out, "exactly one of --group and --principal") {
		t.Errorf("grant without a subject = %q", out)
	}
	if out := linea(exitOK, "access", "grant", "--group", "sre", "--role", "engineer"); !strings.Contains(out, "granted engineer in * to group sre") || !strings.Contains(out, "(revision 1)") {
		t.Errorf("grant = %q", out)
	}
	if out := linea(exitError, "access", "grant", "--group", "ops", "--role", "viewer", "--revision", "0"); !strings.Contains(out, "conflict") {
		t.Errorf("grant at a stale revision = %q", out)
	}
	var l api.RoleBindings
	if err := json.Unmarshal([]byte(linea(exitOK, "access", "list", "--format", "json")), &l); err != nil || l.Revision != 1 || len(l.Bindings) != 2 {
		t.Fatalf("list = %+v, %v", l, err)
	}
	if out := linea(exitOK, "access", "list", "--roles"); !strings.Contains(out, "config") || !strings.Contains(out, "group sre") || !strings.Contains(out, "run.submit") {
		t.Errorf("list text = %q", out)
	}
	if out := linea(exitError, "access", "revoke", "--binding", l.Bindings[0].ID); !strings.Contains(out, "forbidden") {
		t.Errorf("revoking a server config binding = %q", out)
	}
	if out := linea(exitOK, "access", "revoke", "--binding", l.Bindings[1].ID); !strings.Contains(out, "(revision 2)") {
		t.Errorf("revoke = %q", out)
	}
}
