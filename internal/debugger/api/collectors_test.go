package api

import (
	"net/http"
	"testing"

	"github.com/lineahq/linea/internal/debugger/fleetcapture/fleetcapturetest"
	"github.com/lineahq/linea/internal/debugger/platform"
)

func TestCollectorRoutesExposeBoundedSharedInventory(t *testing.T) {
	fleet := fleetcapturetest.NewServer(t)
	service := platform.New(platform.Options{Capture: platform.CaptureOptions{Fleet: platform.FleetOptions{Endpoint: fleet.URL, Token: fleetcapturetest.Token}}})
	t.Cleanup(service.Close)
	_, server := newServerOptions(t, Options{Platform: service})
	response := get(t, server.URL+"/api/v1/collectors")
	if response.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("inventory response may be cached")
	}
	page := decodeBody[platform.CollectorPage](t, response, http.StatusOK)
	if len(page.Items) != 1 || page.Items[0].Pod != "pod" {
		t.Fatalf("inventory %+v", page)
	}
	points := decodeBody[platform.CapturePointPage](t, get(t, server.URL+"/api/v1/collectors/"+page.Items[0].ID+"/capture-points"), http.StatusOK)
	if len(points.Items) != 1 || points.Items[0].Point != "egress" {
		t.Fatalf("points %+v", points)
	}
	for _, query := range []string{"?limit=-1", "?limit=nope", "?limit=101", "?cursor=broken"} {
		body := decodeBody[OperationError](t, get(t, server.URL+"/api/v1/collectors"+query), http.StatusBadRequest)
		if body.Code != "invalid_request" {
			t.Fatalf("query %s: %+v", query, body)
		}
	}
	body := decodeBody[OperationError](t, get(t, server.URL+"/api/v1/collectors/missing"), http.StatusNotFound)
	if body.Code != "not_found" {
		t.Fatal(body)
	}
}
