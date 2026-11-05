package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/lineahq/linea/collector/provider"
	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/mock"
	"github.com/lineahq/linea/internal/debugger/mock/builtin"
	"github.com/lineahq/linea/internal/debugger/registry"
	"github.com/lineahq/linea/internal/debugger/topology"
)

// forwarderProvider serves the forwarder's control endpoint, answering its
// registration, and reports what it saw.
func forwarderProvider() provider.Provider {
	return provider.Provider{
		Name:       "forwarder",
		Components: []provider.Type{{Kind: "extension", Type: "forwarder"}},
		Endpoints:  []provider.Endpoint{{Path: "control"}},
		Prepare: func(_ context.Context, run provider.Run) error {
			var s struct {
				Greeting string `json:"greeting"`
			}
			if err := run.Settings(&s); err != nil {
				return err
			}
			for _, c := range run.Components() {
				if c.Config["control"] == nil {
					return fmt.Errorf("%s has no control configuration: %v", c.ID, c.Config)
				}
				l, err := run.Listener(c.ID, "control")
				if err != nil {
					return err
				}
				var registrations, inputs atomic.Int32
				srv := grpc.NewServer(grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
					if m, _ := grpc.MethodFromServerStream(stream); m == "/forwarder.v1.Control/Register" {
						registrations.Add(1)
					}
					var in emptypb.Empty
					if err := stream.RecvMsg(&in); err != nil {
						return err
					}
					return stream.SendMsg(&emptypb.Empty{})
				}))
				go func() { _ = srv.Serve(l) }()
				run.OnInput(func(context.Context, provider.Input) error {
					inputs.Add(1)
					return nil
				})
				run.OnInputsDone(func(context.Context) error {
					run.Supplied(c.ID, fmt.Sprintf("%d registrations, %d inputs, greeting %s", registrations.Load(), inputs.Load(), s.Greeting))
					ld := plog.NewLogs()
					ld.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords().AppendEmpty().Body().SetStr("from the provider")
					raw, err := plogotlp.NewExportRequestFromLogs(ld).MarshalProto()
					if err != nil {
						return err
					}
					return run.Record(c.ID, "control", "logs", raw)
				})
				run.OnClose(func() error {
					srv.Stop()
					return nil
				})
				run.Limitation(c.ID, "the control service answers registrations only")
			}
			return nil
		},
		Check: func(run provider.Static) []provider.Finding {
			var s struct {
				Greeting string `json:"greeting"`
			}
			if err := run.Settings(&s); err != nil || s.Greeting == "" {
				var pipelines []string
				for _, p := range run.Pipelines() {
					pipelines = append(pipelines, p.Signal+":"+p.ID)
					for _, proc := range p.Processors {
						pipelines = append(pipelines, proc.ID)
					}
				}
				return []provider.Finding{{Component: run.Components()[0].ID, Message: "no greeting", Hint: "set mocks.providers.forwarder.greeting",
					Settings: map[string]any{"greeting": "hello " + strings.Join(pipelines, ",")}}}
			}
			return nil
		},
	}
}

func withForwarderProvider(t *testing.T) {
	t.Helper()
	withForwarder(t)
	previous := providersOf
	t.Cleanup(func() { providersOf = previous })
	providersOf = func() []registry.Provider {
		return []registry.Provider{{Package: "example.com/forwarder/debug", New: forwarderProvider}}
	}
}

// TestAModuleProviderDrivesItsComponent: the provider serves the
// extension's control endpoint, sees the input, records what it received
// as an endpoint output and its evidence as the extension's declaration;
// the topology names it.
func TestAModuleProviderDrivesItsComponent(t *testing.T) {
	withForwarderProvider(t)
	spec := mock.Spec{Providers: map[string]json.RawMessage{"forwarder": json.RawMessage(`{"greeting":"hi"}`)}}
	r := runCompleted(t, Options{ConfigYAML: forwarderYAML, Inputs: []Input{logInput("captured")}, Mocks: spec})

	var decl *mock.Declaration
	for i, d := range r.Mocks {
		if d.Provider == "forwarder" {
			decl = &r.Mocks[i]
		}
	}
	if decl == nil || decl.Ref != "extension/forwarder" || decl.Mode != topology.ModeDependencyMocked ||
		len(decl.Supplied) != 1 || decl.Supplied[0] != "1 registrations, 1 inputs, greeting hi" ||
		len(decl.Limitations) != 1 || len(decl.Warnings) != 0 {
		t.Fatalf("declaration %+v (all %+v)", decl, r.Mocks)
	}
	var control *mock.SimulatedEndpoint
	for i, s := range r.SimulatedEndpoints {
		if s.Path == "control" {
			control = &r.SimulatedEndpoints[i]
		}
	}
	if control == nil || control.Provider != "forwarder" || control.Refused != nil {
		t.Errorf("control endpoint %+v", control)
	}
	var recorded bool
	for _, o := range r.EndpointOutputs {
		if o.Component == "extension/forwarder" && o.Endpoint == "control" && strings.Contains(string(o.Data), "from the provider") {
			recorded = true
		}
	}
	if !recorded {
		t.Errorf("endpoint outputs %+v lack what the provider recorded", r.EndpointOutputs)
	}

	factories, err := factoriesOf()
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := config.ResolveSource(context.Background(), config.Source{YAML: forwarderYAML}, factories, nil)
	if err != nil {
		t.Fatal(err)
	}
	g := topology.Build(resolved.Config, ExecutionModes(resolved.Config, factories))
	for _, c := range g.Components {
		if c.Ref == "extension/forwarder" && (c.Mode != topology.ModeDependencyMocked || !strings.Contains(c.Note, "provider forwarder")) {
			t.Errorf("%s: mode %s, note %q", c.Ref, c.Mode, c.Note)
		}
	}
	if got := DrivingProviders(resolved.Config); len(got) != 1 || got["extension/forwarder"] != "forwarder" {
		t.Errorf("driving providers %v", got)
	}
	if gaps := mock.Gaps(resolved.Config, mock.Spec{}, builtin.Providers, mustPlugins(t)); len(gaps) != 1 || gaps[0].Message != "no greeting" ||
		gaps[0].Provider != "forwarder" || string(gaps[0].Settings) != `{"greeting":"hello logs:logs,processor/tap"}` {
		t.Errorf("gaps without settings %+v (settings %s)", gaps, gaps[0].Settings)
	}
	if gaps := mock.Gaps(resolved.Config, spec, builtin.Providers, mustPlugins(t)); len(gaps) != 0 {
		t.Errorf("gaps with settings %+v", gaps)
	}

	ref, err := RunReference(context.Background(), Options{ConfigYAML: forwarderYAML, Inputs: []Input{logInput("captured")}, Mocks: spec})
	if err != nil {
		t.Fatal(err)
	}
	if ref.Status != runresult.StatusCompleted {
		t.Fatalf("reference run %s: %s", ref.Status, ref.StatusReason)
	}
}

// TestProviderSettingsNameARuntimeProvider: settings for a provider the
// runtime lacks fail the run, naming the providers it has.
func TestProviderSettingsNameARuntimeProvider(t *testing.T) {
	withForwarderProvider(t)
	spec := mock.Spec{Providers: map[string]json.RawMessage{"livecapture": json.RawMessage(`{}`)}}
	_, err := Run(context.Background(), Options{ConfigYAML: forwarderYAML, Inputs: []Input{logInput("x")}, Mocks: spec})
	if err == nil || !strings.Contains(err.Error(), "livecapture") || !strings.Contains(err.Error(), "it has forwarder") {
		t.Fatalf("err = %v", err)
	}
}

// TestAFailingProviderFailsTheRunNamingIt: an error or a panic in a hook
// fails the run with the provider's name and package.
func TestAFailingProviderFailsTheRunNamingIt(t *testing.T) {
	withForwarder(t)
	previous := providersOf
	t.Cleanup(func() { providersOf = previous })
	for name, prepare := range map[string]func(context.Context, provider.Run) error{
		"error": func(context.Context, provider.Run) error { return fmt.Errorf("no session") },
		"panic": func(_ context.Context, run provider.Run) error {
			run.OnReady(func(context.Context) error { panic("boom") })
			return nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			providersOf = func() []registry.Provider {
				return []registry.Provider{{Package: "example.com/forwarder/debug", New: func() provider.Provider {
					return provider.Provider{Name: "forwarder", Components: []provider.Type{{Kind: "extension", Type: "forwarder"}}, Prepare: prepare}
				}}}
			}
			r, err := Run(context.Background(), Options{ConfigYAML: forwarderYAML, Inputs: []Input{logInput("x")}})
			msg := ""
			switch {
			case err != nil:
				msg = err.Error()
			case r.Status == runresult.StatusFailed:
				msg = r.StatusReason
			}
			if !strings.Contains(msg, "provider forwarder (example.com/forwarder/debug)") {
				t.Fatalf("err %v, result %+v", err, r)
			}
		})
	}
}

// TestProposedSettingsMustBeAnObject: settings a check proposes that do
// not marshal into a JSON object are not offered, and a gap says so.
func TestProposedSettingsMustBeAnObject(t *testing.T) {
	withForwarder(t)
	previous := providersOf
	t.Cleanup(func() { providersOf = previous })
	providersOf = func() []registry.Provider {
		return []registry.Provider{{Package: "example.com/forwarder/debug", New: func() provider.Provider {
			return provider.Provider{Name: "forwarder", Components: []provider.Type{{Kind: "extension", Type: "forwarder"}},
				Check: func(run provider.Static) []provider.Finding {
					return []provider.Finding{{Component: run.Components()[0].ID, Message: "no greeting", Settings: []string{"hello"}}}
				}}
		}}}
	}
	factories, err := factoriesOf()
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := config.ResolveSource(context.Background(), config.Source{YAML: forwarderYAML}, factories, nil)
	if err != nil {
		t.Fatal(err)
	}
	gaps := mock.Gaps(resolved.Config, mock.Spec{}, builtin.Providers, mustPlugins(t))
	if len(gaps) != 2 || gaps[1].Settings != nil || gaps[1].Provider != "" ||
		!strings.Contains(gaps[0].Message, "the provider forwarder proposed settings that are not a JSON object") {
		t.Errorf("gaps %+v", gaps)
	}
}

func mustPlugins(t *testing.T) []mock.Plugin {
	t.Helper()
	p, err := Plugins()
	if err != nil {
		t.Fatal(err)
	}
	return p
}
