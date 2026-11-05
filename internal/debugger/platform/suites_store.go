package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
)

type suiteMemberMetadata struct {
	Label string `json:"label"`
	RunID string `json:"run_id,omitempty"`
	Error string `json:"error,omitempty"`
}

type suiteMetadata struct {
	Caller  Actor                 `json:"caller"`
	Created time.Time             `json:"created"`
	Config  string                `json:"config"`
	Members []suiteMemberMetadata `json:"members"`
}

func (s *Service) persistSuite(ctx context.Context, suite Suite) error {
	m := suiteMetadata{Caller: suite.Caller, Created: suite.Created, Config: suite.Config, Members: make([]suiteMemberMetadata, len(suite.Members))}
	for i, member := range suite.Members {
		m.Members[i] = suiteMemberMetadata(member)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = s.repository.SaveSuite(ctx, suite.ID, suite.Project, raw)
	return err
}

func (s *Service) recoverSuites() error {
	records, err := s.repository.Suites(context.Background())
	if err != nil {
		return err
	}
	// Store listings are newest first; the service keeps admission order.
	for i := len(records) - 1; i >= 0; i-- {
		if err := s.loadSuiteMembers(context.Background(), records[i]); err != nil {
			return err
		}
		suite, err := s.suiteFromRecordLocked(records[i])
		if err != nil {
			return err
		}
		s.addSuiteLocked(suite)
	}
	return nil
}

// suiteFromRecordLocked decodes a stored suite whose member runs are known.
// s.mu is held, or the service is not yet started.
func (s *Service) suiteFromRecordLocked(r artifact.SuiteRecord) (Suite, error) {
	var m suiteMetadata
	if err := decodeStored(r.Metadata, &m); err != nil {
		return Suite{}, fmt.Errorf("%w: suite metadata: %w", artifact.ErrCorrupt, err)
	}
	if m.Caller.validate() != nil || m.Created.IsZero() || m.Config == "" || len(m.Members) == 0 || len(m.Members) > MaxSuiteMembers {
		return Suite{}, artifact.ErrCorrupt
	}
	suite := Suite{ID: r.ID, Project: r.Project, Caller: m.Caller, Created: m.Created, Config: m.Config, Members: make([]SuiteMember, len(m.Members))}
	for i, member := range m.Members {
		if member.Label == "" || (member.RunID == "") == (member.Error == "") {
			return Suite{}, artifact.ErrCorrupt
		}
		if member.RunID != "" {
			run, exists := s.runs[member.RunID]
			if !exists || run.mode != ModeScenario {
				return Suite{}, artifact.ErrCorrupt
			}
		}
		suite.Members[i] = SuiteMember(member)
	}
	return suite, nil
}

// addSuiteLocked caches a stored suite. s.mu is held, or the service is not
// yet started.
func (s *Service) addSuiteLocked(suite Suite) {
	for _, m := range suite.Members {
		if m.RunID != "" {
			s.suiteRuns[m.RunID] = true
		}
	}
	s.suites[suite.ID] = suite
	s.suiteOrder = append(s.suiteOrder, suite.ID)
}
