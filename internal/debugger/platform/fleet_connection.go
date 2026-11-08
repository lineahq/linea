package platform

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/lineahq/linea/internal/debugger/authz"
	"github.com/lineahq/linea/internal/debugger/fleetcapture"
)

// FleetConnection is the result of checking linea-server's fleet connection
// now. Configured is false when the server was started without a fleet; then
// Checks is empty.
type FleetConnection struct {
	Configured bool         `json:"configured"`
	Endpoint   string       `json:"endpoint,omitempty"`
	CheckedAt  time.Time    `json:"checked_at"`
	Checks     []FleetCheck `json:"checks"`
}

// FleetCheck is one step of a connection check, in the order they ran:
// "fleet" (reach fleet with the server's credential), "live_capture" (reach
// the livecapture plugin through fleet) and "config_management". Status is
// ok, failed, off (fleet answered and the feature is not enabled) or skipped
// (an earlier step failed). A failed step carries the platform error code.
type FleetCheck struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Message    string `json:"message"`
	Code       Code   `json:"code,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}

// Fleet check statuses.
const (
	FleetCheckOK      = "ok"
	FleetCheckFailed  = "failed"
	FleetCheckOff     = "off"
	FleetCheckSkipped = "skipped"
)

// CheckFleetConnection contacts fleet now and reports each step. A failing
// step is part of the result, not an error: the operation fails only for the
// caller (authorization, cancellation) or a closed platform.
func (s *Service) CheckFleetConnection(ctx context.Context, caller Caller) (_ *FleetConnection, err error) {
	defer s.audit(&caller, "CheckFleetConnection", &err)()
	if err := s.begin(ctx, caller, authz.FleetRead); err != nil {
		return nil, err
	}
	v := &FleetConnection{CheckedAt: s.now().UTC(), Checks: []FleetCheck{}}
	if err := s.collectors.checkFleet(); err != nil {
		if s.collectors.fleet != nil || s.collectors.invalid {
			return nil, err
		}
		return v, nil
	}
	v.Configured = true
	if m, ok := s.collectors.fleet.(fleetManager); ok {
		v.Endpoint = m.Endpoint()
	}
	step := func(name string, run func() (string, string, error)) bool {
		started := time.Now()
		status, message, err := run()
		c := FleetCheck{Name: name, Status: status, Message: message, DurationMS: time.Since(started).Milliseconds()}
		if err != nil {
			if ctx.Err() != nil {
				return false
			}
			c.Status, c.Message, c.Code = FleetCheckFailed, err.Error(), CodeUnavailable
			if e, ok := err.(*Error); ok {
				c.Message, c.Code = e.Message, e.Code
			}
		}
		v.Checks = append(v.Checks, c)
		return c.Status != FleetCheckFailed
	}
	var server struct {
		Version          string `json:"version"`
		Storage          string `json:"storage"`
		ConfigManagement bool   `json:"config_management"`
	}
	reached := step("fleet", func() (string, string, error) {
		if err := s.manage(ctx, caller, http.MethodGet, "/api/v1/server", nil, fleetcapture.NoRevision, nil, &server); err != nil {
			return "", "", err
		}
		return FleetCheckOK, fmt.Sprintf("linea-fleet %s with %s storage accepted the server's credential", server.Version, server.Storage), nil
	})
	if err := ctx.Err(); err != nil {
		return nil, classify(err, CodeCancelled)
	}
	if !reached {
		for _, name := range []string{"live_capture", "config_management"} {
			v.Checks = append(v.Checks, FleetCheck{Name: name, Status: FleetCheckSkipped, Message: "fleet did not answer"})
		}
		return v, nil
	}
	step("live_capture", func() (string, string, error) {
		snap, err := s.collectors.snapshot(ctx, true)
		if err != nil {
			return "", "", err
		}
		return FleetCheckOK, fmt.Sprintf("the livecapture plugin answered; %d Collector(s) connected", len(snap.ids)), nil
	})
	step("config_management", func() (string, string, error) {
		if !server.ConfigManagement {
			return FleetCheckOff, "fleet runs without config management; start fleet with --config-management", nil
		}
		return FleetCheckOK, "fleet keeps config versions, assignments and rollouts", nil
	})
	if err := ctx.Err(); err != nil {
		return nil, classify(err, CodeCancelled)
	}
	return v, nil
}
