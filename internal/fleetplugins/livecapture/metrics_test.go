package livecapture

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/pluginsdk"
)

func TestMetricsCredentialsAndPublicSnapshotIsolation(t *testing.T) {
	s, _, p := fixture(t)
	s.config.metricsToken = strings.Repeat("e", 64)
	call := func(path, token string, bound bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		if bound {
			r.Header.Set(pluginsdk.CallerIDHeader, "owner")
			r.Header.Set(pluginsdk.CallerRoleHeader, "reader")
			r.Header.Set(pluginsdk.InstanceHeader, s.instance)
			r.Header.Set(pluginsdk.EpochHeader, s.epoch)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	if call("/metrics", s.config.serviceToken, false).Code != 401 || call(pluginsdk.APIPath+"v1/metrics", s.config.metricsToken, true).Code != 401 {
		t.Fatal("metrics credential crossed service authority")
	}
	public := call(pluginsdk.APIPath+"v1/metrics", s.config.serviceToken, true)
	var snapshot MetricSnapshot
	if public.Code != 200 || json.Unmarshal(public.Body.Bytes(), &snapshot) != nil || snapshot.Gauges["peers"] != 1 || snapshot.Limits["control_connections"] != 272 {
		t.Fatal("public aggregate snapshot failed")
	}
	private := call("/metrics", s.config.metricsToken, false)
	if private.Code != 200 || !strings.Contains(private.Body.String(), "linea_fleet_livecapture_limit_peers 256") {
		t.Fatal("private Prometheus scrape failed")
	}
	s.mu.Lock()
	completed := make(chan *httptest.ResponseRecorder, 1)
	go func() { completed <- call("/metrics", s.config.metricsToken, false) }()
	var contended *httptest.ResponseRecorder
	select {
	case contended = <-completed:
	case <-time.After(500 * time.Millisecond):
	}
	s.mu.Unlock()
	if contended == nil {
		<-completed
		t.Fatal("scrape blocked behind authority/storage work")
	}
	if contended.Code != 200 || !strings.Contains(contended.Body.String(), "linea_fleet_livecapture_metadata_snapshot_stale 1") {
		t.Fatal("contended scrape did not expose snapshot staleness")
	}
	for _, secret := range []string{s.config.serviceToken, s.config.metricsToken, p.info.ID, s.instance, s.repo.AuthorityID()} {
		if strings.Contains(private.Body.String()+public.Body.String(), secret) {
			t.Fatal("metric cardinality or secret containment failed")
		}
	}
	// Scrapes must remain available when the host is unavailable; they may not
	// revive authorization or require an execution/data-plane round trip.
	s.hostOK = false
	if call("/metrics", s.config.metricsToken, false).Code != 200 {
		t.Fatal("metrics lost during host outage")
	}
}
