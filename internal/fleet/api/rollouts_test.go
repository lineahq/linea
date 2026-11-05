package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/fleet/configmgmt"
	"github.com/lineahq/linea/internal/fleet/fleet"
)

type fakeRollouts struct {
	fakeConfigs
	assignment configmgmt.RolloutRequest
	created    configmgmt.RolloutRequest
	control    configmgmt.Control
	outcome    configmgmt.Outcome
	err        error
}

func (f *fakeRollouts) CreateAssignment(_ context.Context, _ string, a configmgmt.Assignment, r configmgmt.RolloutRequest) (configmgmt.Assignment, configmgmt.RolloutView, error) {
	f.assignment = r
	a.Version, a.RolloutID, a.Revision = 0, r.ID, 1
	return a, configmgmt.RolloutView{ID: r.ID}, f.err
}
func (f *fakeRollouts) CreateRollout(_ context.Context, _ string, r configmgmt.RolloutRequest) (configmgmt.RolloutView, error) {
	f.created = r
	return configmgmt.RolloutView{ID: r.ID, AssignmentID: r.AssignmentID, ToVersion: r.ToVersion, Policy: r.Policy}, f.err
}
func (f *fakeRollouts) ControlRollout(_ context.Context, _, id string, c configmgmt.Control) (configmgmt.RolloutView, error) {
	f.control = c
	return configmgmt.RolloutView{ID: id}, f.err
}
func (f *fakeRollouts) ListRolloutTargets(_ context.Context, _ string, o configmgmt.Outcome, _ string, _ int) (configmgmt.RolloutTargetPage, error) {
	f.outcome = o
	return configmgmt.RolloutTargetPage{Items: []configmgmt.RolloutTarget{}}, nil
}

func TestRolloutRoutes(t *testing.T) {
	repo := &fakeRollouts{}
	h := NewAuthorized(fleet.NewStore(3), roles{}, ServerInfo{ServerID: "test"}, nil, nil)
	if w := call(t, h, "GET", "/api/v1/rollouts", "admin", nil); w.Code != 404 || errorCode(t, w) != "config_management_disabled" {
		t.Fatalf("disabled: %d %s", w.Code, w.Body)
	}
	h.SetConfigManagement(repo, nil)
	assignment, rollout := strings.Repeat("e1", 16), strings.Repeat("f1", 16)

	// Creating an assignment starts its rollout with the default policy.
	w := call(t, h, "POST", "/api/v1/config-assignments", "admin", map[string]any{"id": assignment, "name": "base", "config_id": strings.Repeat("c1", 16), "version": 1, "layer": 10, "selector": "a=b"})
	if w.Code != 201 || !fleet.ValidID(repo.assignment.ID) || repo.assignment.BakeSeconds != configmgmt.DefaultPolicy().BakeSeconds || len(repo.assignment.Waves) != 4 {
		t.Fatalf("create assignment: %d %s %+v", w.Code, w.Body, repo.assignment)
	}
	w = call(t, h, "POST", "/api/v1/config-assignments", "admin", map[string]any{"id": assignment, "name": "base", "config_id": strings.Repeat("c1", 16), "version": 1, "layer": 10, "selector": "a=b",
		"rollout": map[string]any{"id": rollout, "bake_seconds": 5, "waves": []map[string]int{{"percent": 100}}}})
	if w.Code != 201 || repo.assignment.ID != rollout || repo.assignment.BakeSeconds != 5 || repo.assignment.WaveTimeoutSeconds != configmgmt.DefaultPolicy().WaveTimeoutSeconds ||
		len(repo.assignment.Waves) != 1 || repo.assignment.Waves[0] != (configmgmt.Wave{Percent: 100}) {
		t.Fatalf("create assignment with policy: %d %s %+v", w.Code, w.Body, repo.assignment)
	}
	for _, m := range []string{"PATCH", "DELETE"} {
		if w := call(t, h, m, "/api/v1/config-assignments/"+assignment, "admin", map[string]int{"version": 2}); w.Code != 405 {
			t.Fatalf("%s assignment: %d", m, w.Code)
		}
	}

	// Rollouts change versions; policy fields sit beside the request.
	w = call(t, h, "POST", "/api/v1/rollouts", "admin", map[string]any{"assignment_id": assignment, "to_version": 2, "max_failures": 1})
	var view configmgmt.RolloutView
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &view) != nil || view.ToVersion != 2 || repo.created.MaxFailures != 1 || repo.created.MaxFailurePercent != configmgmt.DefaultPolicy().MaxFailurePercent ||
		w.Header().Get("Location") != "/api/v1/rollouts/"+repo.created.ID {
		t.Fatalf("create rollout: %d %s %+v", w.Code, w.Body, repo.created)
	}
	if w := call(t, h, "POST", "/api/v1/rollouts", "reader", map[string]any{"assignment_id": assignment, "delete": true}); w.Code != 403 {
		t.Fatalf("reader rollout: %d", w.Code)
	}
	if w := call(t, h, "POST", "/api/v1/rollouts", "admin", map[string]any{"assignment_id": assignment, "delete": true, "unknown": 1}); w.Code != 400 {
		t.Fatalf("unknown field: %d", w.Code)
	}
	for err, code := range map[error]string{configmgmt.ErrRolloutActive: "rollout_conflict", configmgmt.ErrTooManyTargets: "too_many_targets", fleet.ErrInvalid: "invalid_argument"} {
		repo.err = err
		if w := call(t, h, "POST", "/api/v1/rollouts", "admin", map[string]any{"assignment_id": assignment, "delete": true}); errorCode(t, w) != code {
			t.Fatalf("%v: %d %s", err, w.Code, w.Body)
		}
	}

	// Controls, targets and unknown actions.
	repo.err = configmgmt.ErrRolloutState
	if w := call(t, h, "POST", "/api/v1/rollouts/"+rollout+":resume", "admin", nil); w.Code != 409 || errorCode(t, w) != "invalid_state" || repo.control != configmgmt.ControlResume {
		t.Fatalf("resume: %d %s", w.Code, w.Body)
	}
	repo.err = nil
	if w := call(t, h, "POST", "/api/v1/rollouts/"+rollout+":rollback", "admin", nil); w.Code != 200 || repo.control != configmgmt.ControlRollback {
		t.Fatalf("rollback: %d %s", w.Code, w.Body)
	}
	if w := call(t, h, "POST", "/api/v1/rollouts/"+rollout+":pause", "reader", nil); w.Code != 403 {
		t.Fatalf("reader pause: %d", w.Code)
	}
	if w := call(t, h, "POST", "/api/v1/rollouts/"+rollout+":explode", "admin", nil); w.Code != 404 {
		t.Fatalf("unknown control: %d", w.Code)
	}
	if w := call(t, h, "GET", "/api/v1/rollouts/"+rollout+"/targets?outcome=failed", "reader", nil); w.Code != 200 || repo.outcome != configmgmt.OutcomeFailed {
		t.Fatalf("targets: %d %s", w.Code, w.Body)
	}
	if w := call(t, h, "GET", "/api/v1/rollouts/"+rollout+"/targets?outcome=bogus", "reader", nil); w.Code != 400 {
		t.Fatalf("bad outcome: %d", w.Code)
	}
}
