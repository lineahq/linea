package mcp

import (
	"context"
	"fmt"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lineahq/linea/internal/debugger/platform"
)

type captureIDInput struct {
	SessionID string `json:"session_id" jsonschema:"ID returned by capture creation or listing."`
}

type captureValidationResult struct{ platform.CaptureValidation }

func (r captureValidationResult) summary() string {
	return fmt.Sprintf("Capture preflight: %d collectors; fleet admits the capture on start.", len(r.Collectors))
}

type captureSessionResult struct{ platform.CaptureSession }

func (r captureSessionResult) summary() string {
	active, finished, unknown := 0, 0, 0
	for _, t := range r.Targets {
		switch {
		case t.State == "active":
			active++
		case t.State == "unknown":
			unknown++
		case t.Terminal:
			finished++
		}
	}
	return fmt.Sprintf("Capture %s: terminal %t; %d targets; %d active; %d finished; %d unknown.", r.State, r.Terminal, len(r.Targets), active, finished, unknown)
}

type captureSessionsResult struct{ platform.CaptureSessionPage }

func (r captureSessionsResult) summary() string {
	return fmt.Sprintf("Capture sessions: %d sessions; more %t.", len(r.Items), r.NextCursor != "")
}

// captureSelectionDefaults leaves the filter and limits optional, as over
// HTTP: an empty filter selects everything and a zero limit takes its default.
func captureSelectionDefaults(s *jsonschema.Schema) {
	optional(s, "filter", "limits", "limits.bytes_per_second", "limits.records_per_second", "limits.total_bytes", "limits.total_records", "limits.record_bytes")
}

func addCaptureTools(server *sdk.Server, service *platform.Service) {
	addToolWith(server, "linea_validate_capture", "Preflight a capture against fleet: resolves Collectors, group or selector to capable peers and checks points, signals, duration and budgets. Raw points need fleet's raw permission. Does not start capture.", captureSelectionDefaults, func(ctx context.Context, in platform.CaptureSelection) (summarized, error) {
		v, err := service.ValidateCaptureRequest(ctx, callerOf(ctx), in)
		if err != nil {
			return nil, err
		}
		return captureValidationResult{*v}, nil
	})
	addToolWith(server, "linea_create_capture_session", "Start a bounded fleet capture on the resolved peers. The roster is fixed at start. Records stream into the debugger; per-target stats come from fleet.", captureSelectionDefaults, func(ctx context.Context, in platform.CaptureSelection) (summarized, error) {
		v, err := service.CreateCaptureSession(ctx, callerOf(ctx), in)
		if err != nil {
			return nil, err
		}
		return captureSessionResult{*v}, nil
	})
	addTool(server, "linea_list_capture_sessions", "List capture sessions; uncertain means the debugger lost track of the fleet capture.", func(ctx context.Context, in collectorPageInput) (summarized, error) {
		v, err := service.ListCaptureSessions(ctx, callerOf(ctx), platform.CollectorPageRequest{Limit: in.Limit, Cursor: in.Cursor})
		if err != nil {
			return nil, err
		}
		return captureSessionsResult{*v}, nil
	})
	addTool(server, "linea_get_capture_session", "Read capture state, per-target fleet stats and received data without changing capture.", func(ctx context.Context, in captureIDInput) (summarized, error) {
		v, err := service.GetCaptureSession(ctx, callerOf(ctx), in.SessionID)
		if err != nil {
			return nil, err
		}
		return captureSessionResult{*v}, nil
	})
	addTool(server, "linea_wait_capture_session", "Wait up to 30 seconds for terminal state. Timeout or disconnect never stops capture.", func(ctx context.Context, in captureIDInput) (summarized, error) {
		parent := ctx
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		v, err := service.WaitCaptureSession(ctx, callerOf(ctx), in.SessionID)
		if ctx.Err() != nil && parent.Err() == nil {
			v, err = service.GetCaptureSession(parent, callerOf(parent), in.SessionID)
		}
		if err != nil {
			return nil, err
		}
		return captureSessionResult{*v}, nil
	})
	addTool(server, "linea_stop_capture_session", "Request stop in fleet. A stopping response is not final; inspect or wait for a terminal state.", func(ctx context.Context, in captureIDInput) (summarized, error) {
		v, err := service.StopCaptureSession(ctx, callerOf(ctx), in.SessionID)
		if err != nil {
			return nil, err
		}
		return captureSessionResult{*v}, nil
	})
}
