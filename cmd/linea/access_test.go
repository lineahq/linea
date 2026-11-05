package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// unboundAuthenticator signs every request in as a user no binding names.
type unboundAuthenticator struct{}

func (unboundAuthenticator) Authenticate(*http.Request) (platform.Principal, error) {
	return platform.Principal{ID: "user:mallory", Kind: platform.PrincipalUser}, nil
}

// TestCLIReportsForbidden checks the CLI against a server that refuses its
// principal: the command exits 1 and prints the platform's refusal.
func TestCLIReportsForbidden(t *testing.T) {
	access, err := platform.NewPolicy(nil)
	if err != nil {
		t.Fatal(err)
	}
	service := platform.New(platform.Options{Access: access})
	t.Cleanup(service.Close)
	h := api.New(api.Options{Platform: service, Authenticator: unboundAuthenticator{}})
	t.Cleanup(h.Close)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"runs", "--server", srv.URL}, &stdout, &stderr); code != exitError {
		t.Fatalf("exit %d, want %d: %s", code, exitError, stderr.String())
	}
	if !strings.Contains(stderr.String(), "forbidden: this operation needs the run.read permission") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}
