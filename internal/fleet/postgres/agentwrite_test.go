package postgres

import (
	"errors"
	"testing"

	"github.com/lineahq/linea/internal/fleet/fleet"
)

func TestAgentWritesYieldToTheWriter(t *testing.T) {
	s := &Store{agentWrites: make(chan struct{}, maxAgentWrites)}
	s.writer = newWriter(s)
	var releases []func()
	for range maxAgentWrites {
		release, err := s.agentWrite()
		if err != nil {
			t.Fatalf("admission below the bound: %v", err)
		}
		releases = append(releases, release)
	}
	if _, err := s.agentWrite(); !errors.Is(err, fleet.ErrBusy) {
		t.Fatalf("admission above the bound: %v", err)
	}
	for _, release := range releases {
		release()
	}
	for range queueSize*3/4 + 1 {
		s.writer.queue <- &op{}
	}
	if _, err := s.agentWrite(); !errors.Is(err, fleet.ErrBusy) {
		t.Fatalf("admission while the writer is backlogged: %v", err)
	}
}
