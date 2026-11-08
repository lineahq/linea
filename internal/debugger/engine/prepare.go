package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"time"

	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/hostfs"
	"github.com/lineahq/linea/internal/debugger/runresult"
	"github.com/lineahq/linea/internal/debugger/scenario"
	"github.com/lineahq/linea/internal/debugger/telemetry"
)

// ErrInputLimit identifies bounded source capture failures, before admission.
var ErrInputLimit = errors.New("execution input capture limit exceeded")

const maxInputBytes int64 = 16 << 20
const maxInputFiles = 128

// preparation is private, immutable after Prepare and never client JSON.
// Keep original source bytes and paths, rather than rewriting scenario YAML.
// Parsing anew for each execution also prevents sharing mutable pdata/mock maps.
type preparation struct {
	identity                             requestIdentity
	files                                map[string][]byte
	fixture, scenario, setup, assertions []byte
	scenarioDir                          string
	distribution                         distribution
	fixtureFile, scenarioFile            string
	// schema is set for decoded bundles so they re-encode to their own bytes.
	schema int
	// runtime is the runtime ID DecodeBundleForRuntime matched: the bundle
	// runs on this out-of-process runtime, whose build differs from the
	// admitting debugger's distribution by design.
	runtime string
	// shared marks inputs copied from another prepared request (ShareInputs):
	// preparing captures only the configuration.
	shared bool
}

type requestIdentity struct {
	sources            [11]string
	labels             [3]string
	refs               [3]string
	compare, normalize bool
	snapshot           *config.Snapshot
}

func (r Request) identity() requestIdentity {
	return requestIdentity{
		sources: [11]string{r.Config, r.ConfigYAML, r.Fixture, r.FixtureJSON,
			r.Scenario, r.ScenarioYAML, r.ScenarioDir, r.Receiver, r.ClientAddress, r.SetupYAML, r.AssertionsYAML},
		labels:  [3]string{r.ConfigLabel, r.FixtureLabel, r.ScenarioLabel},
		refs:    r.Sources.identity(),
		compare: r.Compare, normalize: r.Normalize, snapshot: r.ConfigSnapshot,
	}
}

// Prepare captures and validates the sources used by execution. It does not
// start a Collector, write files or require named artifacts. The returned request
// retains original labels and private captured bytes; changing its sources or
// execution settings requires preparing a fresh request. Runtime ID, logger and
// observer can still be supplied by the scheduler after preparation.
// Config capture has its own 16 MiB/128 URI budget. All other inline/file source
// bytes share a separate 16 MiB/128-file budget; decoded memory is not bounded here.
func Prepare(ctx context.Context, req Request) (Request, error) {
	return PrepareWithEnvironment(ctx, req, config.EnvironmentPolicy{LegacyHost: true})
}

// PrepareWithEnvironment captures with trusted confmap environment policy.
// Existing captures are checked too, so prepared requests cannot bypass policy.
// Host paths are unrestricted.
func PrepareWithEnvironment(ctx context.Context, req Request, policy config.EnvironmentPolicy) (Request, error) {
	return PrepareWithPolicy(ctx, req, Policy{Environment: policy, Paths: unrestricted})
}

// PrepareWithPolicy captures under the composition's policy: every host file
// the request names (config, fixture, scenario, the files the scenario names
// and ${file:...} URIs) opens through its path policy, and a refusal fails
// preparation before the file is opened. Captured bundles are not re-checked
// against the roots: their execution reads no host file.
func PrepareWithPolicy(ctx context.Context, req Request, pol Policy) (Request, error) {
	if err := pol.Validate(); err != nil {
		return Request{}, err
	}
	pol = pol.Clone()
	policy := pol.Environment
	if err := req.Validate(); err != nil {
		return Request{}, err
	}
	if err := ctx.Err(); err != nil {
		return Request{}, err
	}
	if req.prepared != nil && !req.prepared.shared {
		if req.identity() != req.prepared.identity {
			return Request{}, fmt.Errorf("%w: prepared request sources or settings changed", ErrInvalidRequest)
		}
		if req.prepared.runtime == "" {
			if err := req.prepared.distribution.compatible(); err != nil {
				return Request{}, err
			}
		}
		if err := req.ConfigSnapshot.CheckEnvironment(policy); err != nil {
			return Request{}, err
		}
		// A request for a built runtime is checked by its worker, whose
		// factories it names (prepared.runtime is set there).
		if req.Compare && (req.Runtime == nil || req.prepared.runtime != "") {
			c, factories, err := inProcessFactories()
			if err != nil {
				return Request{}, err
			}
			resolved, err := req.ConfigSnapshot.Resolve(ctx, factories)
			if err != nil {
				return Request{}, err
			}
			if err := c.CheckReferencePrerequisites(resolved.Config); err != nil {
				return Request{}, err
			}
		}
		return req, nil
	}
	var p *preparation
	var read func(string) ([]byte, error)
	if req.prepared != nil && req.prepared.shared {
		// Inputs copied from another preparation: capture the configuration
		// only, reading the inputs from the copy.
		p = req.prepared
		p.shared = false
		read = p.read
	} else {
		p = &preparation{files: map[string][]byte{}}
		c := &inputCapture{ctx: ctx, files: p.files, paths: pol.Paths}
		if err := c.inputs(req, p, pol); err != nil {
			return Request{}, err
		}
		read = c.read
	}
	req.prepared = p
	data, err := loadInputs(req, read)
	if err != nil {
		return Request{}, err
	}
	if req.Runtime != nil {
		// A built runtime's components may be unknown here: capture
		// without factories; its worker resolves and validates.
		if req.ConfigSnapshot == nil {
			if req.ConfigSnapshot, err = config.CaptureSnapshot(ctx, req.config(), data.opts.Env, pol.captureOptions(data.opts.Mocks.Files)); err != nil {
				return Request{}, err
			}
		}
		if err := req.ConfigSnapshot.CheckEnvironment(policy); err != nil {
			return Request{}, err
		}
		return finishPreparation(ctx, req, p)
	}
	c, factories, err := inProcessFactories()
	if err != nil {
		return Request{}, err
	}
	var resolved *config.Resolved
	if req.ConfigSnapshot == nil {
		resolved, err = config.CaptureSourceWith(ctx, req.config(), factories, data.opts.Env, pol.captureOptions(data.opts.Mocks.Files))
		if err != nil {
			return Request{}, err
		}
		req.ConfigSnapshot = resolved.Snapshot
	} else {
		resolved, err = req.ConfigSnapshot.Resolve(ctx, factories)
		if err != nil {
			return Request{}, err
		}
	}
	if err := req.ConfigSnapshot.CheckEnvironment(policy); err != nil {
		return Request{}, err
	}
	if req.Compare {
		if err := c.CheckReferencePrerequisites(resolved.Config); err != nil {
			return Request{}, err
		}
	}
	return finishPreparation(ctx, req, p)
}

// finishPreparation records the distribution and the prepared identity.
func finishPreparation(ctx context.Context, req Request, p *preparation) (Request, error) {
	if err := ctx.Err(); err != nil {
		return Request{}, err
	}
	var err error
	p.distribution, err = currentDistribution()
	if err != nil {
		return Request{}, err
	}
	if err := ctx.Err(); err != nil {
		return Request{}, err
	}
	p.identity = req.identity()
	return req, nil
}

// inputs captures the request's fixture or scenario, setup and assertions
// text; loadInputs then captures the files the scenario names.
func (c *inputCapture) inputs(req Request, p *preparation, pol Policy) error {
	var err error
	if req.Fixture != "" {
		if p.fixtureFile, err = pol.Paths.Abs(req.Fixture); err != nil {
			return err
		}
	}
	if req.Scenario != "" {
		if p.scenarioFile, err = pol.Paths.Abs(req.Scenario); err != nil {
			return err
		}
	}
	if req.fixture() {
		p.fixture, err = c.source(req.Fixture, req.FixtureJSON)
	} else {
		p.scenario, err = c.source(req.Scenario, req.ScenarioYAML)
	}
	if err != nil {
		return err
	}
	if !req.fixture() {
		dir := req.ScenarioDir
		if req.Scenario != "" {
			dir = filepath.Dir(p.scenarioFile)
		}
		if dir != "" {
			if p.scenarioDir, err = pol.Paths.Abs(dir); err != nil {
				return err
			}
		}
	}
	if req.SetupYAML != "" {
		if p.setup, err = c.source("", req.SetupYAML); err != nil {
			return err
		}
	}
	if req.AssertionsYAML != "" {
		if p.assertions, err = c.source("", req.AssertionsYAML); err != nil {
			return err
		}
	}
	return nil
}

// ShareInputs returns req carrying a copy of from's captured inputs, so that
// preparing it captures only its own configuration and reads no input again
// (a configuration comparison). req must name the same inputs as from
// (fixture or scenario, setup, assertions, receiver, client address, labels
// and saved revisions); only the configuration may differ. from must be
// prepared.
func ShareInputs(req, from Request) (Request, error) {
	if from.prepared == nil || from.identity() != from.prepared.identity {
		return Request{}, fmt.Errorf("%w: the inputs to share are not prepared", ErrInvalidRequest)
	}
	a, b := req.identity(), from.identity()
	if [9]string(a.sources[2:]) != [9]string(b.sources[2:]) || [2]string(a.labels[1:]) != [2]string(b.labels[1:]) ||
		[2]string(a.refs[1:]) != [2]string(b.refs[1:]) || a.compare != b.compare || a.normalize != b.normalize {
		return Request{}, fmt.Errorf("%w: shared inputs must be the same inputs", ErrInvalidRequest)
	}
	p := *from.prepared
	p.files = maps.Clone(from.prepared.files)
	p.identity = requestIdentity{}
	p.shared = true
	req.prepared = &p
	req.ConfigSnapshot = nil
	return req, nil
}

type inputCapture struct {
	ctx   context.Context
	used  int64
	files map[string][]byte
	paths hostfs.Policy
}

func (c *inputCapture) source(path, text string) ([]byte, error) {
	if path != "" {
		return c.read(path)
	}
	if int64(len(text)) > maxInputBytes-c.used {
		return nil, ErrInputLimit
	}
	c.used += int64(len(text))
	return []byte(text), nil
}

func (c *inputCapture) read(path string) ([]byte, error) {
	if err := c.ctx.Err(); err != nil {
		return nil, err
	}
	abs, err := c.paths.Abs(path)
	if err != nil {
		return nil, err
	}
	if raw, ok := c.files[abs]; ok {
		return raw, nil
	}
	if len(c.files) >= maxInputFiles || int64(len(abs)) > maxInputBytes-c.used {
		return nil, ErrInputLimit
	}
	remaining := maxInputBytes - c.used - int64(len(abs))
	// The policy checks the name and the opened handle: a regular file under
	// an allowed root (or any regular file, unrestricted).
	f, err := c.paths.Open(abs)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > remaining {
		return nil, ErrInputLimit
	}
	raw, err := io.ReadAll(io.LimitReader(f, remaining+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > remaining {
		return nil, ErrInputLimit
	}
	if err := c.ctx.Err(); err != nil {
		return nil, err
	}
	c.used += int64(len(abs)) + int64(len(raw))
	c.files[abs] = raw
	return raw, nil
}

func (p *preparation) read(path string) ([]byte, error) {
	abs, err := hostKey(p.distribution.OS, path)
	if err != nil {
		return nil, err
	}
	raw, ok := p.files[abs]
	if !ok {
		return nil, errors.New("execution input dependency was not captured")
	}
	return bytes.Clone(raw), nil
}

type executionInputs struct {
	opts       runresult.Options
	assertions []scenario.Assertion
	name       string
	rebase     bool
	timeout    time.Duration
}

// loadInputs always parses private captured root bytes, with either a capturing
// reader (Prepare) or a closed reader (Execute) for scenario dependencies.
func loadInputs(req Request, read func(string) ([]byte, error)) (executionInputs, error) {
	data := executionInputs{opts: runresult.Options{ConfigPath: req.Config, ConfigYAML: req.ConfigYAML,
		Logger: req.Logger, RunID: req.RunID, Observer: req.Observer, ConfigSnapshot: req.ConfigSnapshot}}
	p := req.prepared
	var s *scenario.Scenario
	var err error
	if !req.fixture() {
		s, err = scenario.Parse(p.scenario, p.scenarioDir)
		if err != nil {
			if req.Scenario == "" {
				err = fmt.Errorf("%w: scenario_yaml: %v", ErrInvalidRequest, err)
			}
			return data, err
		}
		hostOS := p.distribution.OS
		s = s.WithFileReader(read).WithPathResolver(func(dir, name string) (string, error) {
			return hostAbs(hostOS, dir, name)
		})
		data.assertions, data.name = s.Assertions, s.Name
		fixtures, err := s.Fixtures()
		if err != nil {
			return data, err
		}
		for _, f := range fixtures {
			in, err := input(f.Receiver, f.Payload)
			if err != nil {
				return data, err
			}
			in.ClientAddress, in.Headers, in.After = f.ClientAddress, f.Headers, f.After
			data.opts.Inputs = append(data.opts.Inputs, in)
		}
	} else {
		payload, err := telemetry.Decode(p.fixture)
		if err != nil {
			return data, err
		}
		in, err := input(req.Receiver, payload)
		if err != nil {
			return data, err
		}
		in.ClientAddress = req.ClientAddress
		data.opts.Inputs = []runresult.Input{in}
		if req.AssertionsYAML != "" {
			data.assertions, err = scenario.ParseAssertions(p.assertions)
			if err != nil {
				return data, fmt.Errorf("%w: assertions_yaml: %v", ErrInvalidRequest, err)
			}
		}
		if req.SetupYAML != "" {
			s, err = scenario.ParseSetup(p.setup)
			if err != nil {
				return data, fmt.Errorf("%w: setup_yaml: %v", ErrInvalidRequest, err)
			}
			s = s.WithFileReader(read)
		}
	}
	if s != nil {
		data.opts.Mocks, err = s.MockSpec()
		if err != nil {
			if req.fixture() {
				err = fmt.Errorf("%w: setup_yaml: %w", ErrInvalidRequest, err)
			}
			return data, err
		}
		data.opts.Env, data.opts.Settle = s.Environment, s.Settle()
		data.rebase, data.timeout = s.Rebase(), s.Timeout()
	}
	return data, nil
}
