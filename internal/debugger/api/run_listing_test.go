package api

import (
	"net/http"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

func TestRunListingOmitsTheReport(t *testing.T) {
	_, _, srv := workspaceServer(t, memory.NewData())
	tracesDir := filepath.Join("..", "..", "..", "testdata", "traces")
	created := decodeBody[Run](t, post(t, srv.URL+"/api/v1/runs", inlinedFixture(t, filepath.Join(tracesDir, "config.yaml"), filepath.Join(tracesDir, "traces.json"))), http.StatusAccepted)
	decodeBody[engine.Report](t, get(t, srv.URL+"/api/v1/runs/"+created.ID+"/report?wait=true"), http.StatusOK)

	listing := decodeBody[Run](t, get(t, srv.URL+"/api/v1/runs/"+created.ID+"/listing"), http.StatusOK)
	if listing.ID != created.ID || listing.State != StateDone || listing.Finished == nil {
		t.Fatalf("listing = %+v, want the finished run", listing)
	}
	if listing.Report != nil || listing.Summary == nil || listing.Summary.Inputs == 0 {
		t.Fatalf("listing report = %v, summary = %+v; want no report and the list's summary", listing.Report != nil, listing.Summary)
	}
	listed := decodeBody[RunPage](t, get(t, srv.URL+"/api/v1/runs"), http.StatusOK).Runs
	if len(listed) != 1 || !reflect.DeepEqual(listed[0].Summary, listing.Summary) {
		t.Fatal("the listing differs from the run list's entry")
	}
	decodeBody[Error](t, get(t, srv.URL+"/api/v1/runs/missing/listing"), http.StatusNotFound)
}
