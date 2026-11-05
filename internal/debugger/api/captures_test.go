package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/fleetcapture/fleetcapturetest"
	"github.com/lineahq/linea/internal/debugger/platform"
)

func TestCaptureRoutesStrictMutationAndSharedLifecycle(t *testing.T) {
	fleet := fleetcapturetest.NewServer(t)
	service := platform.New(platform.Options{Capture: platform.CaptureOptions{Fleet: platform.FleetOptions{Endpoint: fleet.URL, Token: fleetcapturetest.Token}}})
	t.Cleanup(service.Close)
	_, server := newServerOptions(t, Options{Platform: service})
	postJSON := func(path, body string) *http.Response {
		t.Helper()
		response, err := http.Post(server.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	for _, body := range []string{`{"authorized_raw":true}`, `{} {}`, `null`, strings.Repeat("x", 65<<10)} {
		result := decodeBody[OperationError](t, postJSON("/api/v1/captures", body), http.StatusBadRequest)
		if result.Code != "invalid_request" {
			t.Fatal(result)
		}
	}
	collectors := decodeBody[platform.CollectorPage](t, get(t, server.URL+"/api/v1/collectors"), http.StatusOK)
	if !collectors.Enabled || len(collectors.Items) != 1 {
		t.Fatal(collectors)
	}
	req := `{"collector_ids":["` + collectors.Items[0].ID + `"],"capture_points":[{"pipeline":"logs","point":"egress"}],"signals":["logs"],"filter":{},"limits":{},"duration_seconds":5}`
	validation := decodeBody[platform.CaptureValidation](t, postJSON("/api/v1/captures/validate", req), http.StatusOK)
	if validation.Selection.Limits.TotalRecords != 10000 {
		t.Fatal(validation)
	}
	session := decodeBody[platform.CaptureSession](t, postJSON("/api/v1/captures", req), http.StatusOK)
	response := get(t, server.URL+"/api/v1/captures/"+session.ID)
	if response.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("capture may be cached")
	}
	v := decodeBody[platform.CaptureSession](t, response, http.StatusOK)
	if v.ID != session.ID || v.State != "active" {
		t.Fatal(v)
	}
	page := decodeBody[platform.CaptureSessionPage](t, get(t, server.URL+"/api/v1/captures"), http.StatusOK)
	if len(page.Items) != 1 {
		t.Fatal(page)
	}
	stopped := decodeBody[platform.CaptureSession](t, postJSON("/api/v1/captures/"+session.ID+"/stop", `{}`), http.StatusOK)
	if stopped.State != "stopped" || !stopped.Terminal || stopped.StopReason != "stop_requested" {
		t.Fatal(stopped)
	}
	finished := decodeBody[platform.CaptureSession](t, get(t, server.URL+"/api/v1/captures/"+session.ID+"?wait=true"), http.StatusOK)
	if !finished.Terminal || finished.Targets[0].State != "completed" {
		t.Fatal(finished)
	}
	for _, path := range []string{"/api/v1/capture-policy", "/api/v1/capture-access", "/api/v1/capture-api-keys"} {
		if response := get(t, server.URL+path); response.StatusCode == http.StatusOK {
			t.Fatalf("%s is still served", path)
		}
	}
}
