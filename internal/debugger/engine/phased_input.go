package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/mock"
)

// InspectionInput is what inspection captured on the host: everything a
// runtime needs to complete it without reading host files.
type InspectionInput struct {
	source      Source
	snapshot    *config.Snapshot
	diagnostics []config.Diagnostic
	mocks       mock.Spec
	rebase      bool
}

type inspectionInputJSON struct {
	Source      Source              `json:"source"`
	Snapshot    json.RawMessage     `json:"snapshot"`
	Diagnostics []config.Diagnostic `json:"diagnostics"`
	Mocks       mockSpecJSON        `json:"mocks"`
	Rebase      bool                `json:"rebase,omitempty"`
}

func (in *InspectionInput) MarshalJSON() ([]byte, error) {
	if in == nil || in.snapshot == nil {
		return nil, fmt.Errorf("%w: inspection input is empty", ErrInvalidRequest)
	}
	snap, err := in.snapshot.MarshalJSON()
	if err != nil {
		return nil, err
	}
	return json.Marshal(inspectionInputJSON{Source: in.source, Snapshot: snap, Diagnostics: in.diagnostics, Mocks: mockSpecToJSON(in.mocks), Rebase: in.rebase})
}

func (in *InspectionInput) UnmarshalJSON(raw []byte) error {
	var w inspectionInputJSON
	if err := strictDecode(raw, &w); err != nil {
		return fmt.Errorf("inspection input: %w", err)
	}
	snap, err := config.DecodeSnapshot(w.Snapshot, config.CaptureLimits{})
	if err != nil {
		return fmt.Errorf("inspection input: %w", err)
	}
	*in = InspectionInput{source: w.Source, snapshot: snap, diagnostics: w.Diagnostics, mocks: w.Mocks.spec(), rebase: w.Rebase}
	return nil
}

// OTTLInput is what an OTTL evaluation captured on the host.
type OTTLInput struct {
	snapshot  *config.Snapshot
	processor string
	input     json.RawMessage
}

type ottlInputJSON struct {
	Snapshot  json.RawMessage `json:"snapshot"`
	Processor string          `json:"processor"`
	Input     json.RawMessage `json:"input"`
}

func (in *OTTLInput) MarshalJSON() ([]byte, error) {
	if in == nil || in.snapshot == nil {
		return nil, fmt.Errorf("%w: OTTL input is empty", ErrInvalidRequest)
	}
	snap, err := in.snapshot.MarshalJSON()
	if err != nil {
		return nil, err
	}
	return json.Marshal(ottlInputJSON{Snapshot: snap, Processor: in.processor, Input: in.input})
}

func (in *OTTLInput) UnmarshalJSON(raw []byte) error {
	var w ottlInputJSON
	if err := strictDecode(raw, &w); err != nil {
		return fmt.Errorf("OTTL input: %w", err)
	}
	snap, err := config.DecodeSnapshot(w.Snapshot, config.CaptureLimits{})
	if err != nil {
		return fmt.Errorf("OTTL input: %w", err)
	}
	if len(w.Input) == 0 {
		return fmt.Errorf("OTTL input: input is required")
	}
	*in = OTTLInput{snapshot: snap, processor: w.Processor, input: bytes.Clone(w.Input)}
	return nil
}

func strictDecode(raw []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("unexpected trailing data")
	}
	return nil
}

// mockSpecJSON is the wire form of mock.Spec, kept here so internal/debugger/mock
// carries no JSON tags.
type mockSpecJSON struct {
	Files   []mockFileJSON   `json:"files,omitempty"`
	Objects []map[string]any `json:"objects,omitempty"`
	Drain   time.Duration    `json:"drain,omitempty"`
	Clock   time.Time        `json:"clock,omitzero"`
	// Providers are the module providers' settings.
	Providers map[string]json.RawMessage `json:"providers,omitempty"`
}

type mockFileJSON struct {
	Path     string    `json:"path"`
	Existing []byte    `json:"existing,omitempty"`
	Content  []byte    `json:"content,omitempty"`
	Modified time.Time `json:"modified,omitzero"`
}

func mockSpecToJSON(s mock.Spec) mockSpecJSON {
	w := mockSpecJSON{Objects: s.Objects, Drain: s.Drain, Clock: s.Clock, Providers: s.Providers}
	for _, f := range s.Files {
		w.Files = append(w.Files, mockFileJSON{Path: f.Path, Existing: f.Existing, Content: f.Content, Modified: f.Modified})
	}
	return w
}

func (w mockSpecJSON) spec() mock.Spec {
	s := mock.Spec{Objects: w.Objects, Drain: w.Drain, Clock: w.Clock, Providers: w.Providers}
	for _, f := range w.Files {
		s.Files = append(s.Files, mock.File{Path: f.Path, Existing: f.Existing, Content: f.Content, Modified: f.Modified})
	}
	return s
}
