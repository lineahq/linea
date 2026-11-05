package engine

import (
	"cmp"
	"maps"
	"slices"

	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/release"
)

// Provenance records what is needed to reproduce a run: the exact
// debugger and Collector versions, the component modules, feature gates, the
// hashed inputs and the run settings. Environment values are omitted because
// they may be secrets; only their names are recorded.
type Provenance struct {
	ProductVersion   string                `json:"product_version"`
	CollectorVersion string                `json:"collector_version"`
	Modules          []string              `json:"modules"`
	FeatureGates     []release.FeatureGate `json:"feature_gates"`
	Config           Source                `json:"config"`
	Fixture          *Source               `json:"fixture,omitempty"`
	Scenario         *Source               `json:"scenario,omitempty"`
	// Setup is the mocks and environment of a fixture run, when it has any.
	Setup *Source `json:"setup,omitempty"`
	// Assertions is the separate assertion source of a fixture run, when present.
	Assertions  *Source  `json:"assertions,omitempty"`
	Environment []string `json:"environment"`
	Settings    Settings `json:"settings"`
}

// Settings are the request options that change execution.
type Settings struct {
	Receiver string `json:"receiver,omitempty"`
	Timeout  string `json:"timeout,omitempty"`
	Compare  bool   `json:"compare"`
}

func provenance(req Request, env map[string]string) (*Provenance, error) {
	p := &Provenance{
		ProductVersion:   release.ProductVersion,
		CollectorVersion: release.CollectorVersion,
		Modules:          slices.Clone(release.Modules),
		FeatureGates:     changedFeatureGates(),
		Environment:      slices.Sorted(maps.Keys(env)),
		Settings:         Settings{Receiver: req.Receiver, Compare: req.Compare},
	}
	if p.Environment == nil {
		p.Environment = []string{}
	}
	var err error
	// Execute prepares first, so the snapshot and captured bytes are set and
	// the unrestricted fallbacks below read no host file.
	if req.ConfigSnapshot != nil {
		p.Config = sourceFromBytes(cmp.Or(req.Config, req.ConfigLabel), req.ConfigSnapshot.SourceBytes())
	} else {
		if p.Config, err = sourceOf(req.config(), unrestricted); err != nil {
			return nil, err
		}
		p.Config.Path = cmp.Or(p.Config.Path, req.ConfigLabel)
	}
	inputs := []struct {
		src   config.Source
		label string
		dst   **Source
	}{
		{config.Source{Path: req.Fixture, YAML: req.FixtureJSON}, req.FixtureLabel, &p.Fixture},
		{config.Source{Path: req.Scenario, YAML: req.ScenarioYAML}, req.ScenarioLabel, &p.Scenario},
		{config.Source{YAML: req.SetupYAML}, "", &p.Setup},
		{config.Source{YAML: req.AssertionsYAML}, "", &p.Assertions},
	}
	for i, in := range inputs {
		src, dst := in.src, in.dst
		if src == (config.Source{}) {
			continue
		}
		var s Source
		if req.prepared != nil {
			raw := [4][]byte{req.prepared.fixture, req.prepared.scenario, req.prepared.setup, req.prepared.assertions}[i]
			s = sourceFromBytes(src.Path, raw)
		} else {
			s, err = sourceOf(src, unrestricted)
			if err != nil {
				return nil, err
			}
		}
		s.Path = cmp.Or(s.Path, in.label)
		*dst = &s
	}
	return p, nil
}
