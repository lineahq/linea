package plugins

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/fleet/fleet"
)

func TestMetricsDoesNotWaitForLifecycleAuthority(t *testing.T) {
	r := New(context.Background(), []Config{fixtureConfig("")}, fleet.NewStore(1))
	defer r.Close()
	r.entries[0].mu.Lock()
	done := make(chan string, 1)
	go func() { var out bytes.Buffer; r.WriteMetrics(&out); done <- out.String() }()
	var text string
	select {
	case text = <-done:
	case <-time.After(500 * time.Millisecond):
	}
	r.entries[0].mu.Unlock()
	if text == "" {
		<-done
		t.Fatal("metrics blocked behind lifecycle persistence")
	}
	if !strings.Contains(text, "linea_fleet_plugins{state=\"unknown\"} 1") {
		t.Fatal("contended readiness was presented as known")
	}
}
