package api

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// POST /runs/{id}/statements/test tests a proposal on the run's recorded
// configuration and inputs, and maps the platform's codes.
func TestStatementTestOnRunUsesRecordedInputs(t *testing.T) {
	s, srv := newServerOptions(t, Options{})
	created := decodeBody[Run](t, post(t, srv.URL+"/api/v1/runs", inlined(t, multisignalConfig, multisignalScenario)), http.StatusAccepted)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	waitRun(ctx, t, s.runs, created.ID)
	config, err := os.ReadFile(multisignalConfig)
	if err != nil {
		t.Fatal(err)
	}
	route := srv.URL + "/api/v1/runs/" + created.ID + "/statements/test"

	tested := decodeBody[StatementTest](t, post(t, route, StatementTestRequest{ProposedYAML: string(config)}), http.StatusAccepted)
	if !tested.Candidate.Valid || tested.Run == nil || tested.Run.ID == created.ID {
		t.Fatalf("tested = %+v", tested)
	}
	waitRun(ctx, t, s.runs, tested.Run.ID)
	inputs, err := s.runs.GetRunInputs(ctx, caller, tested.Run.ID)
	if err != nil || inputs.Mode != platform.ModeScenario {
		t.Fatalf("tested run inputs = %+v, %v", inputs, err)
	}

	invalid := decodeBody[StatementTest](t, post(t, route, StatementTestRequest{ProposedYAML: "service: [unclosed"}), http.StatusOK)
	if invalid.Candidate.Valid || invalid.Run != nil || len(invalid.Candidate.Diagnostics) == 0 {
		t.Fatalf("invalid = %+v", invalid)
	}
	for _, tc := range []struct {
		url    string
		body   any
		status int
		code   platform.Code
	}{
		{route, StatementTestRequest{}, http.StatusBadRequest, platform.CodeInvalidRequest},
		{route, StatementTestRequest{ProposedYAML: string(config), AssertionsYAML: "assertions: [unclosed"}, http.StatusBadRequest, platform.CodeInvalidRequest},
		{strings.Replace(route, created.ID, "missing", 1), StatementTestRequest{ProposedYAML: string(config)}, http.StatusNotFound, platform.CodeNotFound},
	} {
		if e := decodeBody[OperationError](t, post(t, tc.url, tc.body), tc.status); e.Code != string(tc.code) {
			t.Fatalf("%+v: code = %q (%s)", tc.body, e.Code, e.Error)
		}
	}
}
