package platform

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/lineahq/linea/internal/debugger/fleetcapture/fleetcapturetest"
)

func checkStatuses(c *FleetConnection) []string {
	out := []string{}
	for _, step := range c.Checks {
		out = append(out, step.Name+"="+step.Status)
	}
	return out
}

// TestCheckFleetConnectionReportsEachStep checks a working connection, a
// refused credential, an unreachable fleet and a server without fleet; a
// failing step is part of the answer, not an error.
func TestCheckFleetConnectionReportsEachStep(t *testing.T) {
	ctx, me := context.Background(), Local("test")
	fleet := fleetcapturetest.NewServer(t)
	gone := httptest.NewServer(nil)
	gone.Close()
	for name, tc := range map[string]struct {
		options CaptureOptions
		want    []string
		code    Code
	}{
		"connected": {CaptureOptions{Fleet: FleetOptions{Endpoint: fleet.URL, Token: fleetcapturetest.Token}},
			[]string{"fleet=ok", "live_capture=ok", "config_management=ok"}, ""},
		"refused credential": {CaptureOptions{Fleet: FleetOptions{Endpoint: fleet.URL, Token: "wrong"}},
			[]string{"fleet=failed", "live_capture=skipped", "config_management=skipped"}, CodeUnavailable},
		"unreachable": {CaptureOptions{Fleet: FleetOptions{Endpoint: gone.URL, Token: fleetcapturetest.Token}},
			[]string{"fleet=failed", "live_capture=skipped", "config_management=skipped"}, CodeUnavailable},
		"not configured": {CaptureOptions{}, []string{}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			s := New(Options{Capture: tc.options})
			t.Cleanup(s.Close)
			v, err := s.CheckFleetConnection(ctx, me)
			if err != nil {
				t.Fatal(err)
			}
			got := checkStatuses(v)
			if v.Configured != (tc.options.Fleet.Endpoint != "") || len(got) != len(tc.want) || v.CheckedAt.IsZero() {
				t.Fatalf("connection = %+v", v)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("checks = %v, want %v", got, tc.want)
				}
			}
			if len(v.Checks) > 0 && v.Checks[0].Code != tc.code {
				t.Fatalf("first check = %+v", v.Checks[0])
			}
			if v.Configured && v.Endpoint == "" {
				t.Fatal("no endpoint")
			}
		})
	}
}
