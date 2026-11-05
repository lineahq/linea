package platform

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/hostfs"
)

func TestOperationErrorsPreserveStructuredDiagnostics(t *testing.T) {
	cause := &config.DiagnosticsError{Diagnostics: []Diagnostic{{Severity: "error", Code: "invalid_configuration", Message: "configuration finding"}}}
	err := classify(fmt.Errorf("wrapped: %w", cause), CodeValidationFailed)
	wantCode(t, err, CodeValidationFailed)
	e := err.(*Error)
	if len(e.Diagnostics) != 1 || e.Diagnostics[0] != cause.Diagnostics[0] || !errors.Is(err, cause) {
		t.Fatalf("classified error lost diagnostic or cause: %+v", e)
	}
	cause.Diagnostics[0].Message = "changed after classification"
	if e.Diagnostics[0].Message != "configuration finding" {
		t.Fatal("operation error aliases its cause's diagnostic slice")
	}
	// Contract §9: validation_failed always carries diagnostics; a cause
	// without findings becomes one error diagnostic with its message.
	plain := classify(errors.New("scenario: unknown field"), CodeValidationFailed).(*Error)
	if want := []Diagnostic{{Severity: config.SeverityError, Code: config.CodeInvalidConfiguration, Message: "scenario: unknown field"}}; !slices.Equal(plain.Diagnostics, want) {
		t.Fatalf("plain validation failure diagnostics = %+v", plain.Diagnostics)
	}
	for _, code := range []Code{CodeInternal, CodeNotFound, CodeInvalidRequest} {
		if d := classify(errors.New("plain error"), code).(*Error).Diagnostics; len(d) != 0 {
			t.Fatalf("%s invented diagnostics: %+v", code, d)
		}
	}
	if d := classify(fmt.Errorf("wrapped: %w", context.Canceled), CodeValidationFailed).(*Error).Diagnostics; len(d) != 0 {
		t.Fatalf("a classified cancellation invented diagnostics: %+v", d)
	}
	s := New(Options{})
	t.Cleanup(s.Close)
	for _, mode := range []InputMode{ModeQuick, ModeScenario} {
		t.Run(string(mode), func(t *testing.T) {
			req := RunRequest{Mode: mode, Config: Source{Text: "receivers: {not_supported: {}}"}}
			if mode == ModeQuick {
				req.Quick = &QuickInput{Fixture: Source{Text: `{"resourceSpans":[]}`}}
			} else {
				req.Scenario = &ScenarioInput{Scenario: Source{Text: "version: 1\ninputs: [{data: '{\"resourceSpans\":[]}'}]\n"}}
			}
			_, err := s.SubmitRun(context.Background(), Local("mcp"), req)
			wantCode(t, err, CodeValidationFailed)
			if len(err.(*Error).Diagnostics) == 0 {
				t.Fatalf("invalid configuration lost structured findings: %v", err)
			}
		})
	}
}

// TestPathRefusalsNameThePolicysGrant: a refused host path tells the caller
// to send the content inline and, when the composition names one, how its
// owner allows the path; a policy without a grant names no setting.
func TestPathRefusalsNameThePolicysGrant(t *testing.T) {
	for grant, want := range map[string]string{
		"add its directory to the allowed roots": "send the content inline, or add its directory to the allowed roots",
		"":                                       "send the content inline",
	} {
		_, err := hostfs.Policy{Grant: grant}.Open("/srv/config.yaml")
		e := classify(err, CodeValidationFailed).(*Error)
		if e.Code != CodeForbidden || !strings.HasSuffix(e.Message, "; "+want) {
			t.Fatalf("grant %q: %s %s", grant, e.Code, e.Message)
		}
	}
}
