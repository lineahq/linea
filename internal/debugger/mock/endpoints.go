package mock

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/configauth"
	"go.opentelemetry.io/collector/config/configgrpc"
	"go.opentelemetry.io/collector/config/confighttp"
	"go.opentelemetry.io/collector/config/configoptional"
	"go.opentelemetry.io/collector/config/configtls"
	"go.opentelemetry.io/collector/pipeline"

	"github.com/lineahq/linea/internal/debugger/telemetry"
	"github.com/lineahq/linea/internal/debugger/topology"
)

// Protocols of simulated endpoints.
const (
	ProtocolGRPC = "grpc"
	ProtocolHTTP = "http"
)

// EndpointsProvider names simulated endpoints in mock declarations.
const EndpointsProvider = "endpoints"

// Client is a client endpoint a component's configuration declares: a
// configgrpc.ClientConfig or confighttp.ClientConfig with an endpoint.
type Client struct {
	// Path is where the client configuration sits in the component's
	// configuration, as its keys: "delivery", "routes[0]::client". An
	// embedded client (squashed, as in otlpexporter.Config) is at its
	// parent's path.
	Path     string
	Protocol string
	// Endpoint is the configured endpoint.
	Endpoint string
}

// Clients returns the client endpoints cfg declares, in field order.
// Clients without an endpoint are left out: they connect nowhere.
func Clients(cfg component.Config) []Client {
	var out []Client
	if _, err := redirected(cfg, func(c Client) (string, error) {
		out = append(out, c)
		return "", nil
	}); err != nil {
		return nil
	}
	return out
}

// EndpointRef is one client endpoint of one component.
type EndpointRef struct {
	Kind topology.Kind
	ID   component.ID
	Path string
}

// Ref is the component's topology ref.
func (r EndpointRef) Ref() string { return topology.Ref(r.Kind, r.ID) }

// EndpointHost is how simulated endpoints hand the run what they receive.
// The runtime implements it.
type EndpointHost interface {
	EndpointReceived(e EndpointRef, p telemetry.Payload)
}

// SimulatedEndpoint is a client endpoint a run pointed at a loopback sink.
// It is part of the run result.
type SimulatedEndpoint struct {
	// Component is the topology ref of the component that declares it.
	Component string `json:"component"`
	// Path is where its client configuration sits in the component's
	// configuration.
	Path     string `json:"path"`
	Protocol string `json:"protocol"`
	// Endpoint is the configured endpoint, which the run never reached.
	Endpoint string `json:"endpoint"`
	// Refused are the calls the sink does not serve, with how often the
	// component made each.
	Refused []RefusedCall `json:"refused,omitempty"`
	// Provider names the provider that served the endpoint itself instead
	// of an OTLP sink; empty for a sink.
	Provider string `json:"provider,omitempty"`
}

// RefusedCall is a call a simulated endpoint answered as not served: a
// gRPC method outside the OTLP export services, or an HTTP request
// outside the OTLP paths.
type RefusedCall struct {
	// Call is the gRPC method ("/pkg.Service/Method") or the HTTP method
	// and path ("GET /api/v1/sessions").
	Call  string `json:"call"`
	Count int    `json:"count"`
}

// Endpoints simulates the client endpoints of components that run for
// real from custom modules: Redirect hands a factory a copy of the
// component's configuration whose clients point at loopback OTLP sinks.
// The Collector configuration is never changed. One Endpoints serves one
// run; Close stops its sinks.
type Endpoints struct {
	host EndpointHost

	mu     sync.Mutex
	sinks  map[EndpointRef]*sink
	order  []EndpointRef
	served map[EndpointRef]*served
	closed bool
}

// served is an endpoint a provider serves on its own listener.
type served struct {
	provider string
	addr     string
	// client is what the configuration declares there, once the run
	// created the component: a client's protocol and endpoint, or an
	// address field (ProtocolAddress).
	client *Client
}

// ProtocolAddress is a served endpoint's protocol when the provider's path
// is a string field holding an address rather than a gRPC or HTTP client.
const ProtocolAddress = "address"

// NewEndpoints returns the simulated endpoints of a run. host may be nil
// (a reference run): what the sinks receive is then dropped.
func NewEndpoints(host EndpointHost) *Endpoints {
	return &Endpoints{host: host, sinks: map[EndpointRef]*sink{}, served: map[EndpointRef]*served{}}
}

// Serve points the endpoint at ref at addr, a provider's listener, instead
// of an OTLP sink. It must be called before the run creates the component.
func (e *Endpoints) Serve(ref EndpointRef, provider, addr string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return errors.New("simulated endpoints are closed")
	}
	if _, ok := e.served[ref]; ok {
		return fmt.Errorf("%s endpoint %s is already served", ref.Ref(), ref.Path)
	}
	e.served[ref] = &served{provider: provider, addr: addr}
	return nil
}

// Connected reports whether the run pointed the component at ref at its
// provider's listener: false while the component was not created or when
// its configuration has nothing at the path.
func (e *Endpoints) Connected(ref EndpointRef) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.served[ref]
	return s != nil && s.client != nil
}

// Received hands the run what a served endpoint received, as a sink does.
func (e *Endpoints) Received(ref EndpointRef, p telemetry.Payload) {
	if e.host != nil {
		e.host.EndpointReceived(ref, p)
	}
}

// servedPaths returns the served endpoints of one component by path.
func (e *Endpoints) servedPaths(kind topology.Kind, id component.ID) map[string]*served {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out map[string]*served
	for ref, s := range e.served {
		if ref.Kind == kind && ref.ID == id {
			if out == nil {
				out = map[string]*served{}
			}
			out[ref.Path] = s
		}
	}
	return out
}

// use records what the configuration declares at a served endpoint and
// returns the provider's address.
func (e *Endpoints) use(s *served, c Client) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if s.client == nil {
		s.client = &c
	}
	return s.addr
}

// Redirect returns a copy of cfg whose client endpoints point at this
// run's sinks, started on first use and shared by every instance of the
// component: TLS off, auth and middlewares removed (the extensions they
// name are inert), an HTTP proxy removed, headers kept. cfg itself is
// unchanged; without clients, cfg is returned as is.
//
// An endpoint a provider serves (Serve) points at the provider's listener
// instead: a client as at a sink, a string field holding an address with
// its host and port replaced.
func (e *Endpoints) Redirect(kind topology.Kind, id component.ID, cfg component.Config) (component.Config, error) {
	byPath := e.servedPaths(kind, id)
	if len(byPath) == 0 && len(Clients(cfg)) == 0 {
		return cfg, nil
	}
	w := walker{holds: map[reflect.Type]bool{}, target: func(c Client) (string, error) {
		if s := byPath[c.Path]; s != nil {
			return e.use(s, c), nil
		}
		s, err := e.sink(EndpointRef{Kind: kind, ID: id, Path: c.Path}, c)
		if err != nil {
			return "", err
		}
		return s.addr, nil
	}}
	for path, s := range byPath {
		if w.addresses == nil {
			w.addresses = map[string]func(string) string{}
		}
		w.addresses[path] = func(old string) string {
			return replaceHost(old, e.use(s, Client{Path: path, Protocol: ProtocolAddress, Endpoint: old}))
		}
	}
	return w.redirect(cfg)
}

// replaceHost replaces the host and port of an address with addr, keeping
// a URL's scheme and path ("http://coordinator:8080/v1" becomes
// "http://127.0.0.1:<port>/v1").
func replaceHost(old, addr string) string {
	if strings.Contains(old, "://") {
		if u, err := url.Parse(old); err == nil {
			u.Host = addr
			return u.String()
		}
	}
	return addr
}

func (e *Endpoints) sink(ref EndpointRef, c Client) (*sink, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, errors.New("simulated endpoints are closed")
	}
	if s := e.sinks[ref]; s != nil {
		return s, nil
	}
	s, err := startSink(ref, c, e.host)
	if err != nil {
		return nil, fmt.Errorf("simulate %s endpoint %s: %w", ref.Ref(), c.Path, err)
	}
	e.sinks[ref] = s
	e.order = append(e.order, ref)
	return s, nil
}

// Simulated lists the endpoints the run simulated, ordered by component
// and path, with the calls each refused.
func (e *Endpoints) Simulated() []SimulatedEndpoint {
	e.mu.Lock()
	refs := slices.Clone(e.order)
	sinks := make([]*sink, len(refs))
	for i, r := range refs {
		sinks[i] = e.sinks[r]
	}
	e.mu.Unlock()
	out := make([]SimulatedEndpoint, len(refs))
	for i, s := range sinks {
		out[i] = SimulatedEndpoint{Component: refs[i].Ref(), Path: refs[i].Path, Protocol: s.client.Protocol,
			Endpoint: s.client.Endpoint, Refused: s.refusedCalls()}
	}
	e.mu.Lock()
	for ref, s := range e.served {
		if s.client != nil {
			out = append(out, SimulatedEndpoint{Component: ref.Ref(), Path: ref.Path, Protocol: s.client.Protocol,
				Endpoint: s.client.Endpoint, Provider: s.provider})
		}
	}
	e.mu.Unlock()
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Component != out[j].Component {
			return out[i].Component < out[j].Component
		}
		return out[i].Path < out[j].Path
	})
	return out
}

// Declarations describe the simulated endpoints as mocks, one per
// component.
func (e *Endpoints) Declarations() []Declaration {
	var out []Declaration
	for _, s := range e.Simulated() {
		if s.Provider != "" {
			continue // Its provider declares it.
		}
		if len(out) == 0 || out[len(out)-1].Ref != s.Component {
			out = append(out, Declaration{Provider: EndpointsProvider, Ref: s.Component, Mode: topology.ModeDependencyMocked,
				Limitations: []string{"client endpoints are loopback OTLP sinks: TLS, auth and proxies are off; only OTLP export calls are served"}})
		}
		d := &out[len(out)-1]
		d.Supplied = append(d.Supplied, fmt.Sprintf("%s: %s (%s)", s.Path, s.Endpoint, s.Protocol))
		for _, r := range s.Refused {
			d.Warnings = append(d.Warnings, fmt.Sprintf("%s: refused %s (%d call(s))", s.Path, r.Call, r.Count))
		}
	}
	return out
}

// Close stops every sink. Later Redirect calls fail.
func (e *Endpoints) Close() error {
	e.mu.Lock()
	e.closed = true
	sinks := make([]*sink, 0, len(e.order))
	for _, r := range e.order {
		sinks = append(sinks, e.sinks[r])
	}
	e.mu.Unlock()
	var err error
	for _, s := range sinks {
		err = errors.Join(err, s.close())
	}
	return err
}

var (
	grpcClientType = reflect.TypeFor[configgrpc.ClientConfig]()
	httpClientType = reflect.TypeFor[confighttp.ClientConfig]()
)

// optionalPkg is the package of configoptional.Optional, whose value the
// walk reaches through its Get method.
const optionalPkg = "go.opentelemetry.io/collector/config/configoptional"

// redirected returns a copy of cfg in which each client with an endpoint
// is pointed at the address target returns for it. The copy shares
// everything with cfg except the structs, pointers, slices and maps on the
// way to a client.
func redirected(cfg component.Config, target func(Client) (string, error)) (component.Config, error) {
	return walker{target: target, holds: map[reflect.Type]bool{}}.redirect(cfg)
}

// redirect walks a copy of cfg.
func (w walker) redirect(cfg component.Config) (component.Config, error) {
	v := reflect.ValueOf(cfg)
	if !v.IsValid() || v.Kind() != reflect.Pointer || v.IsNil() {
		return cfg, nil
	}
	cp := reflect.New(v.Type().Elem())
	cp.Elem().Set(v.Elem())
	if err := w.walk(cp.Elem(), ""); err != nil {
		return nil, err
	}
	return cp.Interface().(component.Config), nil
}

type walker struct {
	target func(Client) (string, error)
	// addresses rewrite the string fields at their paths: served
	// endpoints that are not clients.
	addresses map[string]func(old string) string
	holds     map[reflect.Type]bool
}

// walk visits an addressable value, copying each pointer, slice and map
// that can hold a client before descending into it.
func (w walker) walk(v reflect.Value, path string) error {
	t := v.Type()
	if !w.canHold(t) {
		return nil
	}
	switch t.Kind() {
	case reflect.String:
		if rewrite, ok := w.addresses[path]; ok && v.String() != "" {
			v.SetString(rewrite(v.String()))
		}
	case reflect.Struct:
		switch {
		case t == grpcClientType:
			return w.client(v.Addr().Interface().(*configgrpc.ClientConfig), nil, path)
		case t == httpClientType:
			return w.client(nil, v.Addr().Interface().(*confighttp.ClientConfig), path)
		case isOptional(t):
			if p := v.Addr().MethodByName("Get").Call(nil)[0]; !p.IsNil() {
				return w.walk(p.Elem(), path)
			}
			return nil
		}
		for i := range t.NumField() {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			name, ok := fieldKey(f)
			if !ok {
				continue
			}
			if err := w.walk(v.Field(i), join(path, name)); err != nil {
				return err
			}
		}
	case reflect.Pointer:
		if v.IsNil() {
			return nil
		}
		cp := reflect.New(t.Elem())
		cp.Elem().Set(v.Elem())
		v.Set(cp)
		return w.walk(cp.Elem(), path)
	case reflect.Slice:
		if v.IsNil() {
			return nil
		}
		cp := reflect.MakeSlice(t, v.Len(), v.Len())
		reflect.Copy(cp, v)
		v.Set(cp)
		for i := range v.Len() {
			if err := w.walk(v.Index(i), path+"["+strconv.Itoa(i)+"]"); err != nil {
				return err
			}
		}
	case reflect.Array:
		for i := range v.Len() {
			if err := w.walk(v.Index(i), path+"["+strconv.Itoa(i)+"]"); err != nil {
				return err
			}
		}
	case reflect.Map:
		if v.IsNil() {
			return nil
		}
		keys := v.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return fmt.Sprint(keys[i]) < fmt.Sprint(keys[j]) })
		cp := reflect.MakeMapWithSize(t, v.Len())
		for _, k := range keys {
			e := reflect.New(t.Elem()).Elem()
			e.Set(v.MapIndex(k))
			if err := w.walk(e, join(path, fmt.Sprint(k))); err != nil {
				return err
			}
			cp.SetMapIndex(k, e)
		}
		v.Set(cp)
	}
	return nil
}

func (w walker) client(g *configgrpc.ClientConfig, h *confighttp.ClientConfig, path string) error {
	c := Client{Path: path, Protocol: ProtocolGRPC}
	if g != nil {
		c.Endpoint = g.Endpoint
	} else {
		c.Protocol, c.Endpoint = ProtocolHTTP, h.Endpoint
	}
	if c.Endpoint == "" {
		return nil
	}
	addr, err := w.target(c)
	if err != nil || addr == "" {
		return err
	}
	tls := configtls.ClientConfig{Insecure: true}
	none := configoptional.None[configauth.Config]()
	if g != nil {
		g.Endpoint, g.TLS, g.Auth, g.Middlewares = addr, tls, none, nil
		return nil
	}
	h.Endpoint, h.TLS, h.Auth, h.Middlewares, h.ProxyURL = "http://"+addr+httpPath(h.Endpoint), tls, none, nil, ""
	return nil
}

// httpPath is the path and query of an HTTP client endpoint, which the
// sink keeps so the component's requests arrive as it sends them.
func httpPath(endpoint string) string {
	s := endpoint
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return ""
	}
	out := u.EscapedPath()
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	return out
}

// canHold reports whether a value of type t can contain a client
// configuration the walk reaches.
func (w walker) canHold(t reflect.Type) bool {
	if h, ok := w.holds[t]; ok {
		return h
	}
	w.holds[t] = false // breaks cycles
	var h bool
	switch t.Kind() {
	case reflect.String:
		h = len(w.addresses) > 0
	case reflect.Struct:
		switch {
		case t == grpcClientType || t == httpClientType:
			h = true
		case isOptional(t):
			h = w.canHold(optionalValue(t))
		default:
			for i := range t.NumField() {
				f := t.Field(i)
				if _, ok := fieldKey(f); ok && f.IsExported() && w.canHold(f.Type) {
					h = true
					break
				}
			}
		}
	case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
		h = w.canHold(t.Elem())
	}
	w.holds[t] = h
	return h
}

func isOptional(t reflect.Type) bool {
	return t.PkgPath() == optionalPkg && strings.HasPrefix(t.Name(), "Optional[")
}

// optionalValue is T of a configoptional.Optional[T].
func optionalValue(t reflect.Type) reflect.Type {
	m, ok := reflect.PointerTo(t).MethodByName("Get")
	if !ok || m.Type.NumOut() != 1 || m.Type.Out(0).Kind() != reflect.Pointer {
		return reflect.TypeFor[struct{}]()
	}
	return m.Type.Out(0).Elem()
}

// fieldKey is a field's key in the configuration: its mapstructure name,
// or "" for a squashed (or untagged embedded) field, which shares its
// parent's keys. ok is false for a field the configuration skips.
func fieldKey(f reflect.StructField) (string, bool) {
	name, opts, _ := strings.Cut(f.Tag.Get("mapstructure"), ",")
	if name == "-" {
		return "", false
	}
	if slices.Contains(strings.Split(opts, ","), "squash") || (f.Anonymous && name == "") {
		return "", true
	}
	if name == "" {
		name = strings.ToLower(f.Name)
	}
	return name, true
}

func join(path, name string) string {
	switch {
	case name == "":
		return path
	case path == "":
		return name
	}
	return path + "::" + name
}

// signals are the OTLP signals a sink serves.
var signals = []pipeline.Signal{pipeline.SignalTraces, pipeline.SignalMetrics, pipeline.SignalLogs}

// loopback is where sinks listen: never another interface.
var loopback = net.JoinHostPort("127.0.0.1", "0")

// sink is a loopback OTLP server standing for one client endpoint.
type sink struct {
	ref    EndpointRef
	client Client
	host   EndpointHost
	addr   string
	stop   func(context.Context) error

	mu      sync.Mutex
	refused map[string]int
}

func startSink(ref EndpointRef, c Client, host EndpointHost) (*sink, error) {
	s := &sink{ref: ref, client: c, host: host, refused: map[string]int{}}
	var err error
	if c.Protocol == ProtocolGRPC {
		err = s.startGRPC()
	} else {
		err = s.startHTTP()
	}
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (s *sink) received(p telemetry.Payload) {
	if s.host != nil {
		s.host.EndpointReceived(s.ref, p)
	}
}

func (s *sink) refuse(call string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refused[call]++
}

func (s *sink) refusedCalls() []RefusedCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]RefusedCall, 0, len(s.refused))
	for c, n := range s.refused {
		out = append(out, RefusedCall{Call: c, Count: n})
	}
	slices.SortFunc(out, func(a, b RefusedCall) int { return strings.Compare(a.Call, b.Call) })
	if len(out) == 0 {
		return nil
	}
	return out
}

func (s *sink) close() error {
	return s.stop(context.Background())
}
