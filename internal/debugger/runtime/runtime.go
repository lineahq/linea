// Package runtime executes an unchanged Collector configuration in-process
// against fixtures. Receivers are replaced by fixture injection, exporters
// that could reach the network are replaced by local capture, and supported
// processors are instrumented while the real processors execute.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/lineahq/linea/internal/debugger/release"
	"github.com/lineahq/linea/internal/debugger/runresult"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/connector"
	"go.opentelemetry.io/collector/exporter"
	"go.opentelemetry.io/collector/otelcol"
	"go.opentelemetry.io/collector/pipeline"
	"go.opentelemetry.io/collector/processor"
	"go.opentelemetry.io/collector/receiver"
	"go.uber.org/zap"

	"github.com/lineahq/linea/collector/provider"
	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/instrument/tailsampling"
	"github.com/lineahq/linea/internal/debugger/mock"
	"github.com/lineahq/linea/internal/debugger/mock/builtin"
	"github.com/lineahq/linea/internal/debugger/registry"
	"github.com/lineahq/linea/internal/debugger/standin"
	"github.com/lineahq/linea/internal/debugger/telemetry"
	"github.com/lineahq/linea/internal/debugger/topology"
)

var buildInfo = component.BuildInfo{
	Command:     "linea",
	Description: "OpenTelemetry Collector Pipeline Debugger",
	Version:     release.ProductVersion,
}

// factoriesOf returns the registry runs execute with; tests add components
// to it.
var factoriesOf = registry.Factories

// localExporters run unmodified because they cannot send data off the host;
// what reaches them is recorded (observedExporterFactory). Every other
// exporter is replaced by local capture.
var localExporters = []component.Type{component.MustNewType("debug"), component.MustNewType("nop")}

// ExecutionModes is the isolation policy of Run for cfg with the factories
// it resolved with as a topology.ModeFunc: components whose dependency a
// mock provider substitutes are dependency_mocked, custom extensions run
// for real, custom components with client endpoints are
// dependency_mocked, and the rest follow ExecutionMode.
func ExecutionModes(cfg *otelcol.Config, factories otelcol.Factories) topology.ModeFunc {
	plugins, _ := Plugins()
	mocked := mock.Modes(mock.Build(cfg, mock.Spec{}, nil, builtin.Providers, plugins))
	return func(kind topology.Kind, id component.ID) (topology.Mode, string) {
		if c, ok := mocked[topology.Ref(kind, id)]; ok {
			return c.Mode, c.Note
		}
		if isStandIn(cfg, kind, id) {
			return topology.ModeStandIn, standInNotes[kind]
		}
		clients := simulatedClients(cfg, factories, kind, id)
		if kind == topology.KindExtension && isCustom(factories.ExtensionModules, id.Type()) {
			return withEndpointNote(topology.ModeReal, customExtensionNote, clients)
		}
		mode, note := ExecutionMode(kind, id)
		if kind == topology.KindProcessor && note == "" {
			note = timeNote(cfg, id)
		}
		return withEndpointNote(mode, note, clients)
	}
}

// standInNotes say what a stand-in does in a run, per kind.
var standInNotes = map[topology.Kind]string{
	topology.KindReceiver:  "not in the distribution; fixtures are injected where it would deliver data",
	topology.KindProcessor: "not in the distribution; a stand-in passes data through unchanged",
	topology.KindExporter:  "not in the distribution; telemetry is recorded locally",
	topology.KindConnector: "not in the distribution; a stand-in forwards data of its own signal and emits nothing into other signals",
	topology.KindExtension: "not in the distribution; " + extensionNote,
}

// isStandIn reports whether the component's type is outside the
// distribution, so a stand-in runs in its place.
func isStandIn(cfg *otelcol.Config, kind topology.Kind, id component.ID) bool {
	var c component.Config
	switch kind {
	case topology.KindReceiver:
		c = cfg.Receivers[id]
	case topology.KindProcessor:
		c = cfg.Processors[id]
	case topology.KindExporter:
		c = cfg.Exporters[id]
	case topology.KindConnector:
		c = cfg.Connectors[id]
	case topology.KindExtension:
		c = cfg.Extensions[id]
	}
	return standin.Is(c)
}

// withStandIns adds a stand-in factory for every type of the resolved
// configuration that the distribution lacks.
func withStandIns(base otelcol.Factories, resolved *config.Resolved) otelcol.Factories {
	return standin.Add(base, standin.FromConfig(base, resolved.Config))
}

// ExecutionMode is the default isolation policy as a topology.ModeFunc:
// every processor of the distribution is real (and instrumented), receivers
// are replaced by fixture injection, and exporters that could reach the
// network are captured.
func ExecutionMode(kind topology.Kind, id component.ID) (topology.Mode, string) {
	switch kind {
	case topology.KindReceiver:
		return topology.ModeSimulated, "fixtures are injected where the receiver delivers data; its network intake and decoding do not run"
	case topology.KindProcessor:
		return topology.ModeReal, ""
	case topology.KindExporter:
		if slices.Contains(localExporters, id.Type()) {
			return topology.ModeReal, ""
		}
		return topology.ModeCaptured, "telemetry is recorded locally instead of being sent"
	case topology.KindConnector:
		return topology.ModeReal, ""
	case topology.KindExtension:
		return topology.ModeSimulated, extensionNote
	}
	return topology.ModeUnsupported, "the debugger does not execute " + string(kind) + "s yet"
}

// Input and Options are the plain data of one run, in runresult so that
// code preparing a run links no Collector.
type (
	Input   = runresult.Input
	Options = runresult.Options
)

func resolveOptions(ctx context.Context, o Options, factories otelcol.Factories) (*config.Resolved, error) {
	if o.ConfigSnapshot != nil {
		return o.ConfigSnapshot.Resolve(ctx, factories)
	}
	return config.CaptureSource(ctx, config.Source{Path: o.ConfigPath, YAML: o.ConfigYAML}, factories, o.Env, config.CaptureLimits{})
}

// budgetReason is the status reason of a run that otherwise completed but
// reached its evidence budget.
const budgetReason = "evidence budget reached; observations are incomplete"

// truncation returns the truncation of a run side that dropped events and
// bytes since its budget's earlier state, or nil when it dropped nothing.
func truncation(events int, bytes int64, incomplete []string) *runresult.Truncation {
	if events == 0 && bytes == 0 && len(incomplete) == 0 {
		return nil
	}
	return &runresult.Truncation{Reason: runresult.TruncationEvidenceBudget, DroppedEvents: events, DroppedBytes: bytes, IncompleteOutputs: incomplete}
}

// target is an input with its resolved receiver.
type target struct {
	receiver component.ID
	payload  telemetry.Payload
	client   net.Addr
	// clientAddress is client as the input declared it.
	clientAddress string
	// headers are the input's request headers (lowercased names), and
	// keepHeaders whether its receiver hands them to the pipeline.
	headers     map[string][]string
	keepHeaders bool
	after       time.Duration
}

// providerInput is the input as providers' OnInput hooks see it.
func (t target) providerInput(i int) provider.Input {
	return provider.Input{Index: i, Receiver: t.receiver.String(), Signal: t.payload.Signal.String()}
}

// Run executes the configuration with instrumentation and network isolation.
//
// Configuration and startup failures are returned as errors. Once the
// Collector runs, the outcome is a Result whose Status says whether its
// observations are final; a deadline or cancellation of ctx stops the run
// and shuts the Collector down before Run returns.
func Run(ctx context.Context, opts Options) (*runresult.Result, error) {
	logger := opts.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	// Lets decorated processors learn their pipeline ID.
	logger = logger.WithOptions(zap.WrapCore(instrument.WrapCore))
	base, err := factoriesOf()
	if err != nil {
		return nil, err
	}
	resolved, err := resolveOptions(ctx, opts, base)
	if err != nil {
		return nil, err
	}
	base = withStandIns(base, resolved)
	targets, err := resolveTargets(resolved.Config, opts.Inputs, len(opts.Mocks.Files) > 0)
	if err != nil {
		return nil, err
	}

	settle, settleReason := settleFor(resolved.Config, opts.Settle)
	if err := checkWaits(ctx, targets, settle, settleReason); err != nil {
		return nil, err
	}

	rec := instrument.NewRecorder()
	if opts.RunID != "" {
		rec = instrument.NewRecorderWithID(opts.RunID)
	}
	rec.SetObserver(opts.Observer)
	rec.SetBudget(opts.Budget)
	rec.SetDecisionHook(tailsampling.Hook)
	rep := newReplay()
	capt := newCapture(rec, opts.Budget)
	host := newMockHost(rec, rep)
	spec := opts.Mocks
	if spec.Clock.IsZero() {
		spec.Clock = latestRecordTime(targets)
	}
	plugins, err := Plugins()
	if err != nil {
		return nil, err
	}
	if err := mock.CheckSettings(spec, plugins); err != nil {
		return nil, err
	}
	providers := mock.Build(resolved.Config, spec, host, builtin.Providers, plugins)
	received := newEndpointCapture(rec, opts.Budget)
	endpoints := mock.NewEndpoints(received)
	defer endpoints.Close()
	session := mock.NewSession(resolved.Config, spec, providers, endpoints, logger)
	defer session.Close()
	factories, err := isolatedFactories(base, rec, rep, capt, providers, endpoints, logger)
	if err != nil {
		return nil, err
	}
	watcher, err := statusWatcherURIs(resolved.Config, base.ExtensionModules)
	if err != nil {
		return nil, err
	}
	if err := session.Prepare(ctx); err != nil {
		return nil, err
	}

	rec.Record(instrument.Event{Kind: instrument.KindRunStarted, Source: instrument.SourceOrchestrator})
	inputs := newInputResults(targets)
	started, runErr := runCollector(ctx, factories, resolved, append(slices.Clone(resolved.URIs), watcher...), func(ctx context.Context) error {
		if err := session.Ready(ctx); err != nil {
			return err
		}
		for i, t := range targets {
			if err := sleep(ctx, t.after); err != nil {
				return err
			}
			if err := session.Input(ctx, t.providerInput(i)); err != nil {
				return err
			}
			payload := t.payload.Clone()
			seq := rec.RecordTelemetry(instrument.Event{
				Kind:      instrument.KindInput,
				Component: t.receiver.String(),
				Mode:      instrument.ModeSimulated,
				Source:    instrument.SourceReplayReceiver,
			}, payload)
			inputs[i].Injected = true
			ictx := instrument.WithEmission(ctx, seq)
			ictx = t.withClient(ictx)
			if err := rep.inject(ictx, t.receiver, payload); err != nil {
				inputs[i].Error = err.Error()
			}
		}
		if err := session.InputsDone(ctx); err != nil {
			return err
		}
		return sleep(ctx, settle)
	})
	session.Close()
	status, reason := classify(ctx, runErr)
	if !started && status == runresult.StatusFailed {
		return nil, runErr
	}
	if err := rec.Err(); err != nil {
		return nil, err
	}
	// A run that reached its evidence budget still ends with run.completed:
	// the Collector completed and only observation stopped. Its status
	// becomes partial below.
	end := instrument.Event{Kind: instrument.KindRunCompleted, Source: instrument.SourceOrchestrator, Error: reason}
	switch status {
	case runresult.StatusCancelled:
		end.Kind = instrument.KindRunCancelled
	case runresult.StatusPartial, runresult.StatusFailed:
		end.Kind = instrument.KindRunFailed
	}
	rec.Record(end)
	rec.ReleaseObjects()
	metrics := rec.ComponentMetrics(context.WithoutCancel(ctx))

	outputs, incomplete, err := buildOutputs(capt.snapshot())
	if err != nil {
		return nil, err
	}
	endpointOutputs, endpointIncomplete, err := received.outputs()
	if err != nil {
		return nil, err
	}
	incomplete = append(incomplete, endpointIncomplete...)
	droppedEvents, droppedBytes := opts.Budget.Dropped()
	cut := truncation(droppedEvents, droppedBytes, incomplete)
	if cut != nil && status == runresult.StatusCompleted {
		status, reason = runresult.StatusPartial, budgetReason
	}
	inputs = append(inputs, host.inputs()...)
	declared := append(session.Declarations(), endpoints.Declarations()...)
	return &runresult.Result{
		Mode:         runresult.ModeInstrumented,
		RunID:        rec.RunID(),
		Status:       status,
		StatusReason: reason,
		EventSchema:  instrument.EventSchemaVersion,
		Inputs:       inputs,
		Events:       rec.Events(),
		Snapshots:    rec.Snapshots(),
		Outputs:      outputs,
		Mocks:        declared,
		Settle:       settleResult(settle, settleReason),
		Truncation:   cut,

		ComponentMetrics:   metrics,
		EndpointOutputs:    endpointOutputs,
		SimulatedEndpoints: endpoints.Simulated(),
	}, nil
}

// classify derives the run status from the run's context and error. A run
// whose context ended is never reported as completed: its inputs or flushes
// may have been cut short. A context ended with cause DeadlineExceeded (a
// timeout, or a cancellation that names it) counts as a timeout.
func classify(ctx context.Context, runErr error) (runresult.Status, string) {
	switch {
	case errors.Is(context.Cause(ctx), context.DeadlineExceeded):
		return runresult.StatusPartial, "run deadline exceeded; observations are incomplete"
	case ctx.Err() != nil:
		return runresult.StatusCancelled, "run cancelled"
	case runErr != nil:
		return runresult.StatusFailed, runErr.Error()
	}
	return runresult.StatusCompleted, ""
}

func newInputResults(targets []target) []runresult.InputResult {
	out := make([]runresult.InputResult, len(targets))
	for i, t := range targets {
		out[i] = runresult.InputResult{
			Receiver: t.receiver.String(),
			Signal:   t.payload.Signal.String(),
			Counts:   t.payload.Counts(),
		}
		if t.client != nil {
			out[i].ClientAddress = t.clientAddress
		}
		if t.after > 0 {
			out[i].After = t.after.String()
		}
		if len(t.headers) > 0 {
			out[i].Headers = t.headerNames()
			out[i].HeadersDropped = !t.keepHeaders
		}
	}
	return out
}

// resolveTargets resolves each input's receiver. A run needs inputs unless
// mocks supply them (mocked files).
func resolveTargets(cfg *otelcol.Config, inputs []Input, mocked bool) ([]target, error) {
	if len(inputs) == 0 && !mocked {
		return nil, errors.New("no inputs")
	}
	targets := make([]target, len(inputs))
	for i, in := range inputs {
		id, err := injectionTarget(cfg, in)
		if err != nil {
			return nil, fmt.Errorf("input %d: %w", i+1, err)
		}
		targets[i] = target{receiver: id, payload: in.Payload, clientAddress: in.ClientAddress, headers: in.Headers, after: in.After}
		if len(in.Headers) > 0 {
			if targets[i].keepHeaders, err = includesMetadata(cfg, id); err != nil {
				return nil, fmt.Errorf("input %d: %w", i+1, err)
			}
		}
		if in.ClientAddress != "" {
			if targets[i].client, err = ParseClientAddress(in.ClientAddress); err != nil {
				return nil, fmt.Errorf("input %d: %w", i+1, err)
			}
		}
	}
	return targets, nil
}

// ParseClientAddress parses an input's client address: an IP address, or
// an IP address and port.
func ParseClientAddress(s string) (*net.TCPAddr, error) {
	host, port := s, 0
	if h, p, err := net.SplitHostPort(s); err == nil {
		n, err := net.LookupPort("tcp", p)
		if err != nil {
			return nil, fmt.Errorf("client address %q: %w", s, err)
		}
		host, port = h, n
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil, fmt.Errorf("client address %q is not an IP address", s)
	}
	return &net.TCPAddr{IP: ip, Port: port}, nil
}

// latestRecordTime is the latest timestamp a record of the inputs carries,
// or zero when none does. Simulated clocks start there, so a run's output
// does not depend on when it executed.
func latestRecordTime(targets []target) time.Time {
	payloads := make([]telemetry.Payload, len(targets))
	for i, t := range targets {
		payloads[i] = t.payload
	}
	return telemetry.Latest(payloads...)
}

// isolatedFactories wraps the upstream registry: receivers with a mocked
// dependency run against their mock and all other receivers become replay
// receivers, non-local exporters become capture exporters, and all
// processors (after any mock wrapping) and connectors are decorated, and
// extensions are inert unless custom (runExtensions). Custom processors,
// connectors and extensions are created with their client endpoints
// simulated. Everything else is the real factory.
func isolatedFactories(base otelcol.Factories, rec *instrument.Recorder, rep *replay, capt *capture, providers []mock.Provider, endpoints *mock.Endpoints, logger *zap.Logger) (otelcol.Factories, error) {
	out := base
	out.Telemetry = isolatedTelemetry(logger)

	var receivers []receiver.Factory
	for _, f := range unique(base.Receivers) {
		mocked := false
		for _, p := range providers {
			if g, ok := p.Receiver(f); ok {
				f, mocked = g, true
				break
			}
		}
		if mocked {
			f = instrument.Receiver(f, rec)
		} else {
			f = replayReceiverFactory(f, rep)
		}
		receivers = append(receivers, f)
	}
	var processors []processor.Factory
	for _, f := range unique(base.Processors) {
		for _, p := range providers {
			if g, ok := p.Processor(f); ok {
				f = g
				break
			}
		}
		if isCustom(base.ProcessorModules, f.Type()) {
			f = endpoints.Processor(f)
		}
		processors = append(processors, instrument.Processor(f, rec))
	}
	var connectors []connector.Factory
	for _, f := range unique(base.Connectors) {
		for _, p := range providers {
			if g, ok := p.Connector(f); ok {
				f = g
				break
			}
		}
		if isCustom(base.ConnectorModules, f.Type()) {
			f = endpoints.Connector(f)
		}
		connectors = append(connectors, instrument.Connector(f, rec))
	}
	var exporters []exporter.Factory
	for _, f := range unique(base.Exporters) {
		if slices.Contains(localExporters, f.Type()) {
			f = observedExporterFactory(f, capt.observeLocal)
		} else {
			f = captureExporterFactory(f, capt)
		}
		exporters = append(exporters, f)
	}

	var err error
	if out.Receivers, err = otelcol.MakeFactoryMap(receivers...); err != nil {
		return otelcol.Factories{}, err
	}
	if out.Processors, err = otelcol.MakeFactoryMap(processors...); err != nil {
		return otelcol.Factories{}, err
	}
	if out.Exporters, err = otelcol.MakeFactoryMap(exporters...); err != nil {
		return otelcol.Factories{}, err
	}
	if out.Connectors, err = otelcol.MakeFactoryMap(connectors...); err != nil {
		return otelcol.Factories{}, err
	}
	if out.Extensions, err = runExtensions(base, rec, endpoints); err != nil {
		return otelcol.Factories{}, err
	}
	return out, nil
}

// unique returns each factory once; factory maps also hold alias entries.
func unique[T component.Factory](m map[component.Type]T) []T {
	seen := map[component.Type]bool{}
	var out []T
	for _, k := range slices.SortedFunc(maps.Keys(m), func(a, b component.Type) int {
		return strings.Compare(a.String(), b.String())
	}) {
		f := m[k]
		if seen[f.Type()] {
			continue
		}
		seen[f.Type()] = true
		out = append(out, f)
	}
	return out
}

// injectionTarget picks the receiver that would have delivered the fixture:
// the single receiver feeding pipelines of the fixture's signal, or the one
// named in opts.
func injectionTarget(cfg *otelcol.Config, in Input) (component.ID, error) {
	candidates := map[component.ID]bool{}
	for id, p := range cfg.Service.Pipelines {
		if id.Signal() != in.Payload.Signal {
			continue
		}
		for _, r := range p.Receivers {
			// A connector listed as a receiver is fed by another pipeline.
			if _, ok := cfg.Connectors[r]; !ok {
				candidates[r] = true
			}
		}
	}
	if in.Receiver != nil {
		if !candidates[*in.Receiver] {
			return component.ID{}, fmt.Errorf("receiver %s does not feed a %s pipeline", in.Receiver, in.Payload.Signal)
		}
		return *in.Receiver, nil
	}
	switch len(candidates) {
	case 0:
		return component.ID{}, fmt.Errorf("configuration has no %s pipeline for the fixture", in.Payload.Signal)
	case 1:
		for id := range candidates {
			return id, nil
		}
	}
	names := make([]string, 0, len(candidates))
	for id := range candidates {
		names = append(names, id.String())
	}
	sort.Strings(names)
	return component.ID{}, fmt.Errorf("several receivers feed %s pipelines (%v); select one explicitly", in.Payload.Signal, names)
}

// runCollector starts a real otelcol.Collector, calls drive once it is
// running, then shuts it down. Shutdown flushes stateful processors such as
// batch, so all output is delivered before runCollector returns. If ctx ends,
// the Collector shuts itself down; runCollector still waits for it, and calls
// Shutdown only once the Collector is running. started
// reports whether the Collector reached the running state.
func runCollector(ctx context.Context, factories otelcol.Factories, resolved *config.Resolved, uris []string, drive func(context.Context) error) (started bool, err error) {
	col, err := otelcol.NewCollector(otelcol.CollectorSettings{
		BuildInfo:               buildInfo,
		Factories:               func() (otelcol.Factories, error) { return factories, nil },
		ConfigProviderSettings:  resolved.ProviderSettings(uris),
		DisableGracefulShutdown: true,
		SkipSettingGRPCLogger:   true,
	})
	if err != nil {
		return false, err
	}

	done := make(chan error, 1)
	go func() { done <- col.Run(ctx) }()

	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for col.GetState() != otelcol.StateRunning {
		select {
		case err := <-done:
			if err == nil {
				err = errors.New("collector stopped before running")
			}
			return false, fmt.Errorf("start collector: %w", err)
		case <-ctx.Done():
			// Run stops itself once ctx ends. Shutdown must not be called
			// here: Run may not have begun, and in otelcol v0.162.0 a
			// Shutdown before Run races on the Collector's WaitGroup.
			<-done
			return false, ctx.Err()
		case <-ticker.C:
		}
	}

	driveErr := drive(ctx)
	col.Shutdown()
	return true, errors.Join(driveErr, <-done)
}

// captured is one batch delivered to an exporter: its counts, and its
// telemetry when the budget kept it.
type captured struct {
	signal  pipeline.Signal
	counts  telemetry.Counts
	payload *telemetry.Payload
}

// keep charges p's output data (its JSON size) to budget and returns the
// batch, with a copy of p when the budget keeps it.
func keep(budget *instrument.Budget, p telemetry.Payload) captured {
	c := captured{signal: p.Signal, counts: p.Counts()}
	if budget != nil {
		data, err := p.JSON()
		if err == nil && !budget.Charge(int64(len(data)), 0) {
			return c
		}
	}
	clone := p.Clone()
	c.payload = &clone
	return c
}

// buildOutputs concatenates each exporter's batches per signal. Batches and
// counts cover every batch; data covers the kept ones, and an output that
// lost data is listed in the returned "exporter/signal" names.
func buildOutputs(exports map[string][]captured) ([]runresult.Output, []string, error) {
	outputs := []runresult.Output{}
	var incomplete []string
	for _, name := range slices.Sorted(maps.Keys(exports)) {
		bySignal := map[pipeline.Signal][]captured{}
		for _, c := range exports[name] {
			bySignal[c.signal] = append(bySignal[c.signal], c)
		}
		for _, signal := range []pipeline.Signal{pipeline.SignalTraces, pipeline.SignalMetrics, pipeline.SignalLogs} {
			batches := bySignal[signal]
			if len(batches) == 0 {
				continue
			}
			var kept []telemetry.Payload
			var counts telemetry.Counts
			for _, c := range batches {
				counts.Resources += c.counts.Resources
				counts.Scopes += c.counts.Scopes
				counts.Items += c.counts.Items
				if c.payload != nil {
					kept = append(kept, *c.payload)
				}
			}
			merged, ok := telemetry.Concat(kept)
			if !ok {
				merged = telemetry.Empty(signal)
			}
			if len(kept) < len(batches) {
				incomplete = append(incomplete, name+"/"+signal.String())
			}
			data, err := merged.JSON()
			if err != nil {
				return nil, nil, err
			}
			outputs = append(outputs, runresult.Output{
				Exporter: name,
				Signal:   signal.String(),
				Batches:  len(batches),
				Counts:   counts,
				Data:     data,
			})
		}
	}
	return outputs, incomplete, nil
}
