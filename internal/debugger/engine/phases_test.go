package engine

import (
	"errors"
	"testing"
	"time"
)

// TestPhaseTrackerTimesConsecutivePhases: Next ends one phase and starts the
// next, Done ends the last as failed or not, and a nil recorder records
// nothing.
func TestPhaseTrackerTimesConsecutivePhases(t *testing.T) {
	p := NewPhases(time.Now())
	tr := p.Track()
	tr.Next(PhaseConfigResolve)
	tr.Next(PhaseCollectorRun)
	tr.Done(errors.New("collector exited"))
	tr.Done(nil) // nothing is open
	got := p.List()
	if len(got) != 2 || got[0].Name != PhaseConfigResolve || got[0].Failed || got[1].Name != PhaseCollectorRun || !got[1].Failed {
		t.Fatalf("phases %+v", got)
	}
	if got[1].Offset < got[0].Offset+got[0].Duration || got[0].Offset < 0 {
		t.Fatalf("phases overlap: %+v", got)
	}

	var none *Phases
	none.Track().Next(PhaseAnalysis)
	none.Track().Done(nil)
	none.Add(Phase{Name: PhaseAnalysis})
	if none.List() != nil || none.Since(time.Now()) != 0 {
		t.Fatal("a nil recorder recorded")
	}
	if !KnownPhase(PhaseResultUpload) || KnownPhase("collector.config") {
		t.Fatal("KnownPhase")
	}
}
