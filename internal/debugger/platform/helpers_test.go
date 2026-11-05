package platform

import (
	"context"
	"time"

	"github.com/lineahq/linea/internal/debugger/engine"
)

// Scheduler and storage tests drive the engine-level paths directly; adapters
// use the operations.

var testCaller = Local("test")

func modeOf(req engine.Request) InputMode {
	if req.Scenario != "" || req.ScenarioYAML != "" {
		return ModeScenario
	}
	return ModeQuick
}

func (s *Service) Submit(req engine.Request) (*Handle, error) {
	return s.submit(s.ctx, testCaller, modeOf(req), req, time.Time{})
}

func (s *Service) Get(id string) (*Handle, error) { return s.handle(context.Background(), id) }

// List returns every run of every project, newest first.
func (s *Service) List() []Run {
	q, runs := RunQuery{Limit: MaxRunPage}, []Run{}
	for {
		page, err := s.list(s.ctx, q, nil)
		if err != nil {
			panic(err)
		}
		runs = append(runs, page.Runs...)
		if page.NextCursor == "" {
			return runs
		}
		q.Cursor = page.NextCursor
	}
}

func (s *Service) Execute(ctx context.Context, req engine.Request) (*engine.Report, error) {
	return s.execute(ctx, testCaller, modeOf(req), req)
}
