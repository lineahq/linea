package builtin

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/open-telemetry/opentelemetry-collector-contrib/processor/k8sattributesprocessor"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/connector"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/otelcol"
	"go.opentelemetry.io/collector/processor"
	"go.opentelemetry.io/collector/processor/xprocessor"
	"go.opentelemetry.io/collector/receiver"

	"github.com/lineahq/linea/collector/provider"
	"github.com/lineahq/linea/internal/debugger/mock"
	"github.com/lineahq/linea/internal/debugger/topology"
)

// k8sattributesTypes are the processor's type and its deprecated alias: at
// v1.1.0 (the v0.162.0 release) the factory type is k8s_attributes, and
// configurations written as k8sattributes still resolve to it.
var k8sattributesTypes = []component.Type{component.MustNewType("k8s_attributes"), component.MustNewType("k8sattributes")}

// kubeEnvMu serializes processor starts: the processor reads the API
// server address from KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT
// when it starts, and the environment is process-wide.
var kubeEnvMu sync.Mutex

// kubeSettle is how long the fake API server must stay idle before the
// processors count as loaded.
const kubeSettle = 100 * time.Millisecond

// kubeProvider runs the real k8sattributes processor against an in-process
// fake Kubernetes API server that serves the scenario's objects. The
// processor never reaches a real cluster: its API settings are replaced
// with the fake server's address and no authentication.
type kubeProvider struct {
	objects []map[string]any
	ids     []component.ID
	cfgs    map[component.ID]*k8sattributesprocessor.Config

	parsed  []kubeObject
	servers map[component.ID]*fakeKubeAPI
	readyAt time.Time
}

func newKubernetes(cfg *otelcol.Config, spec mock.Spec) *kubeProvider {
	ids := mock.UsedProcessors(cfg, k8sattributesTypes...)
	if len(ids) == 0 {
		return nil
	}
	p := &kubeProvider{objects: spec.Objects, ids: ids, cfgs: map[component.ID]*k8sattributesprocessor.Config{}}
	for _, id := range ids {
		pc, _ := cfg.Processors[id].(*k8sattributesprocessor.Config)
		p.cfgs[id] = pc
	}
	return p
}

func (p *kubeProvider) Name() string { return "kubernetes" }

func (p *kubeProvider) Components() []mock.Component {
	out := make([]mock.Component, 0, len(p.ids))
	for _, id := range p.ids {
		out = append(out, mock.Component{
			Kind: topology.KindProcessor, ID: id, Mode: topology.ModeDependencyMocked,
			Note: "the real processor reads a mocked Kubernetes API serving the scenario's objects (mocks.kubernetes)",
		})
	}
	return out
}

func (p *kubeProvider) Connector(f connector.Factory) (connector.Factory, bool) { return f, false }

func (p *kubeProvider) Receiver(f receiver.Factory) (receiver.Factory, bool) { return f, false }

func (p *kubeProvider) Processor(f processor.Factory) (processor.Factory, bool) {
	if !slices.Contains(k8sattributesTypes, f.Type()) {
		return f, false
	}
	var opts []xprocessor.FactoryOption
	if ap, ok := f.(aliasProvider); ok && ap.DeprecatedAlias().String() != "" {
		opts = append(opts, xprocessor.WithDeprecatedTypeAlias(ap.DeprecatedAlias()))
	}
	opts = append(opts,
		xprocessor.WithTraces(func(ctx context.Context, set processor.Settings, cfg component.Config, next consumer.Traces) (processor.Traces, error) {
			c, api, err := p.redirect(set.ID, cfg)
			if err != nil {
				return nil, err
			}
			proc, err := f.CreateTraces(ctx, set, c, next)
			if err != nil {
				return nil, err
			}
			return kubeTraces{proc, api}, nil
		}, f.TracesStability()),
		xprocessor.WithMetrics(func(ctx context.Context, set processor.Settings, cfg component.Config, next consumer.Metrics) (processor.Metrics, error) {
			c, api, err := p.redirect(set.ID, cfg)
			if err != nil {
				return nil, err
			}
			proc, err := f.CreateMetrics(ctx, set, c, next)
			if err != nil {
				return nil, err
			}
			return kubeMetrics{proc, api}, nil
		}, f.MetricsStability()),
		xprocessor.WithLogs(func(ctx context.Context, set processor.Settings, cfg component.Config, next consumer.Logs) (processor.Logs, error) {
			c, api, err := p.redirect(set.ID, cfg)
			if err != nil {
				return nil, err
			}
			proc, err := f.CreateLogs(ctx, set, c, next)
			if err != nil {
				return nil, err
			}
			return kubeLogs{proc, api}, nil
		}, f.LogsStability()),
	)
	return xprocessor.NewFactory(f.Type(), f.CreateDefaultConfig, opts...), true
}

// redirect returns the copy of cfg the processor runs with: no
// authentication against the fake server, and waiting for its metadata
// at start so the first records are enriched.
func (p *kubeProvider) redirect(id component.ID, cfg component.Config) (component.Config, *fakeKubeAPI, error) {
	orig, ok := cfg.(*k8sattributesprocessor.Config)
	if !ok {
		return nil, nil, fmt.Errorf("unexpected k8sattributes config %T", cfg)
	}
	api := p.servers[id]
	if api == nil {
		return nil, nil, fmt.Errorf("no mocked Kubernetes API for %s", id)
	}
	c := *orig
	c.APIConfig.AuthType = "none"
	c.APIConfig.Context = ""
	c.WaitForMetadata = true
	if c.WaitForMetadataTimeout <= 0 {
		c.WaitForMetadataTimeout = 10 * time.Second
	}
	return &c, api, nil
}

// startWith starts a processor while the environment points at api.
func startWith(api *fakeKubeAPI, start func() error) error {
	kubeEnvMu.Lock()
	defer kubeEnvMu.Unlock()
	host, port := api.hostPort()
	restore := setEnv(map[string]string{"KUBERNETES_SERVICE_HOST": host, "KUBERNETES_SERVICE_PORT": port})
	defer restore()
	return start()
}

func setEnv(values map[string]string) func() {
	type prev struct {
		v  string
		ok bool
	}
	saved := map[string]prev{}
	for k, v := range values {
		old, ok := os.LookupEnv(k)
		saved[k] = prev{old, ok}
		_ = os.Setenv(k, v)
	}
	return func() {
		for k, p := range saved {
			if p.ok {
				_ = os.Setenv(k, p.v)
			} else {
				_ = os.Unsetenv(k)
			}
		}
	}
}

type kubeTraces struct {
	processor.Traces
	api *fakeKubeAPI
}

func (k kubeTraces) Start(ctx context.Context, h component.Host) error {
	return startWith(k.api, func() error { return k.Traces.Start(ctx, h) })
}

type kubeMetrics struct {
	processor.Metrics
	api *fakeKubeAPI
}

func (k kubeMetrics) Start(ctx context.Context, h component.Host) error {
	return startWith(k.api, func() error { return k.Metrics.Start(ctx, h) })
}

type kubeLogs struct {
	processor.Logs
	api *fakeKubeAPI
}

func (k kubeLogs) Start(ctx context.Context, h component.Host) error {
	return startWith(k.api, func() error { return k.Logs.Start(ctx, h) })
}

// Start validates the objects and starts one fake API server per
// processor, so each declaration reports what that processor read.
func (p *kubeProvider) start(context.Context) error {
	parsed, err := parseKubeObjects(p.objects)
	if err != nil {
		return err
	}
	p.parsed = parsed
	p.servers = map[component.ID]*fakeKubeAPI{}
	for _, id := range p.ids {
		p.servers[id] = newFakeKubeAPI(parsed)
	}
	return nil
}

// Ready waits until the processors' informers have loaded the objects:
// processor start only waits for pods, and the other informers (namespaces,
// replica sets, nodes, ...) list concurrently.
func (p *kubeProvider) ready(ctx context.Context) error {
	p.readyAt = time.Now()
	return waitFor(ctx, func() bool {
		for _, api := range p.servers {
			if !api.quiet(p.readyAt, kubeSettle) {
				return false
			}
		}
		return true
	})
}

func (p *kubeProvider) drive(context.Context) error { return nil }

// Gaps reports processors that cannot associate telemetry with the
// scenario's objects: they are invalid, none are declared, or no Pod is.
// Passthrough processors read no objects.
func (p *kubeProvider) Gaps() []mock.Gap {
	parsed, err := parseKubeObjects(p.objects)
	var out []mock.Gap
	for _, id := range p.ids {
		if cfg := p.cfgs[id]; cfg != nil && cfg.Passthrough {
			continue
		}
		ref := topology.Ref(topology.KindProcessor, id)
		switch {
		case err != nil:
			out = append(out, mock.Gap{Ref: ref, Message: "the mocked Kubernetes objects are invalid: " + err.Error()})
		case len(parsed) == 0:
			out = append(out, mock.Gap{Ref: ref,
				Message: "the processor reads Kubernetes metadata, but no Kubernetes objects are mocked, so nothing will be associated",
				Hint:    "declare the pods (and namespaces, owners, nodes) under mocks.kubernetes, for example kubectl get -o yaml output in mocks.kubernetes.files"})
		case !slices.ContainsFunc(parsed, func(o kubeObject) bool { return o.res.Kind == "Pod" }):
			out = append(out, mock.Gap{Ref: ref,
				Message: "the processor associates telemetry with pods, but no Pod objects are mocked",
				Hint:    "add the pods that send the telemetry to mocks.kubernetes"})
		}
	}
	return out
}

func (p *kubeProvider) close() error {
	for _, api := range p.servers {
		api.close()
	}
	return nil
}

func (p *kubeProvider) declarations() []mock.Declaration {
	var out []mock.Declaration
	for _, id := range p.ids {
		d := mock.Declaration{Provider: p.Name(), Ref: topology.Ref(topology.KindProcessor, id), Mode: topology.ModeDependencyMocked}
		d.Limitations = []string{
			"the processor read a mocked Kubernetes API serving the scenario's objects; they do not change during the run",
			"the API connection (auth_type, context) is replaced by the mocked API's",
		}
		if cfg := p.cfgs[id]; cfg == nil || !cfg.WaitForMetadata {
			d.Limitations = append(d.Limitations, "wait_for_metadata is enabled, so the first records are enriched; without it a starting Collector can pass records before its cache is filled")
		}
		api := p.servers[id]
		if cfg := p.cfgs[id]; cfg != nil && cfg.Passthrough {
			d.Limitations = append(d.Limitations, "passthrough mode: the processor adds only the connection IP and reads no objects")
		}
		if api == nil {
			out = append(out, d)
			continue
		}
		objects, kinds := api.served()
		d.Supplied = objects
		if len(p.parsed) == 0 && (p.cfgs[id] == nil || !p.cfgs[id].Passthrough) {
			d.Warnings = append(d.Warnings, "no Kubernetes objects are mocked (mocks.kubernetes), so nothing could be associated")
		}
		for _, k := range kinds {
			if !slices.ContainsFunc(p.parsed, func(o kubeObject) bool { return o.res.Kind == k }) && len(p.parsed) > 0 {
				d.Warnings = append(d.Warnings, "the processor watches "+k+" objects, but none are mocked")
			}
		}
		var unread []string
		for _, o := range p.parsed {
			if !slices.Contains(kinds, o.res.Kind) {
				unread = append(unread, o.String())
			}
		}
		if len(unread) > 0 {
			d.Warnings = append(d.Warnings, "not read, as no extraction rule needs their kind: "+strings.Join(unread, ", "))
		}
		api.mu.Lock()
		for _, u := range api.unserved {
			d.Warnings = append(d.Warnings, "the mocked API does not serve "+u)
		}
		api.mu.Unlock()
		out = append(out, d)
	}
	return out
}

// parseKubeObjects validates decoded manifests. A List (as kubectl get -o
// yaml prints) contributes its items. Namespaced objects without a
// namespace are in "default", as kubectl would create them.
func parseKubeObjects(raw []map[string]any) ([]kubeObject, error) {
	var flat []map[string]any
	for _, o := range raw {
		if items, ok := o["items"].([]any); ok && strings.HasSuffix(fmt.Sprint(o["kind"]), "List") {
			for i, it := range items {
				m, ok := it.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("%v item %d is not an object", o["kind"], i)
				}
				flat = append(flat, m)
			}
			continue
		}
		flat = append(flat, o)
	}
	var out []kubeObject
	seen := map[string]bool{}
	for i, o := range flat {
		kind, _ := o["kind"].(string)
		apiVersion, _ := o["apiVersion"].(string)
		res, ok := kubeResourceOf(kind)
		if !ok {
			return nil, fmt.Errorf("objects[%d]: kind %q is not served; supported: %s", i, kind, supportedKinds())
		}
		if apiVersion != res.apiVersion() {
			return nil, fmt.Errorf("objects[%d]: %s has apiVersion %q, want %s", i, kind, apiVersion, res.apiVersion())
		}
		md, ok := o["metadata"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("objects[%d]: %s has no metadata", i, kind)
		}
		name, _ := md["name"].(string)
		if name == "" {
			return nil, fmt.Errorf("objects[%d]: %s has no metadata.name", i, kind)
		}
		obj := kubeObject{res: res, name: name, raw: cloneMap(o)}
		md = obj.metadata()
		if res.Namespaced {
			ns, _ := md["namespace"].(string)
			if ns == "" {
				ns = "default"
				md["namespace"] = ns
			}
			obj.namespace = ns
		} else {
			delete(md, "namespace")
		}
		if _, ok := md["resourceVersion"]; !ok {
			md["resourceVersion"] = kubeResourceVersion
		}
		if seen[obj.String()] {
			return nil, fmt.Errorf("objects[%d]: duplicate %s", i, obj)
		}
		seen[obj.String()] = true
		out = append(out, obj)
	}
	return out, nil
}

func supportedKinds() string {
	kinds := make([]string, len(kubeResources))
	for i, r := range kubeResources {
		kinds[i] = r.Kind
	}
	return strings.Join(kinds, ", ")
}

func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if mm, ok := v.(map[string]any); ok {
			v = cloneMap(mm)
		}
		out[k] = v
	}
	return out
}

// Prepare runs the provider on the run's lifecycle.
func (p *kubeProvider) Prepare(ctx context.Context, run provider.Run) error {
	return mock.PrepareBuiltin(ctx, run, mock.Lifecycle{Start: p.start, Ready: p.ready, Drive: p.drive, Declarations: p.declarations, Close: p.close})
}
