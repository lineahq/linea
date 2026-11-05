package mock

import (
	"bytes"
	"context"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/configauth"
	"go.opentelemetry.io/collector/config/configgrpc"
	"go.opentelemetry.io/collector/config/confighttp"
	"go.opentelemetry.io/collector/config/configopaque"
	"go.opentelemetry.io/collector/config/configoptional"
	"go.opentelemetry.io/collector/exporter/otlpexporter"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/lineahq/linea/internal/debugger/telemetry"
	"github.com/lineahq/linea/internal/debugger/topology"
)

// clientsConfig declares clients in the shapes custom components use.
type clientsConfig struct {
	Delivery otlpexporter.Config                              `mapstructure:"delivery"`
	Control  *configgrpc.ClientConfig                         `mapstructure:"control"`
	Upload   configoptional.Optional[confighttp.ClientConfig] `mapstructure:"upload"`
	Routes   []route                                          `mapstructure:"routes"`
	Peers    map[string]confighttp.ClientConfig               `mapstructure:"peers"`
	Unused   configgrpc.ClientConfig                          `mapstructure:"unused"`
	Skipped  configgrpc.ClientConfig                          `mapstructure:"-"`
	Name     string                                           `mapstructure:"name"`
}

type route struct {
	Client configgrpc.ClientConfig `mapstructure:"client"`
}

func newClientsConfig() *clientsConfig {
	c := &clientsConfig{Name: "lab"}
	c.Delivery.ClientConfig.Endpoint = "collector.example:4317"
	c.Delivery.ClientConfig.TLS.CAFile = "/etc/ca.pem"
	c.Delivery.ClientConfig.Headers = configopaque.MapList{{Name: "x-tenant", Value: "lab"}}
	c.Delivery.ClientConfig.Auth = configoptional.Some(configauth.Config{AuthenticatorID: component.MustNewID("oauth2client")})
	c.Control = &configgrpc.ClientConfig{Endpoint: "control.example:443"}
	c.Upload = configoptional.Some(confighttp.ClientConfig{Endpoint: "https://upload.example/v1/logs?tenant=lab", ProxyURL: "http://proxy:3128"})
	c.Routes = []route{{Client: configgrpc.ClientConfig{Endpoint: "a.example:4317"}}, {}}
	c.Peers = map[string]confighttp.ClientConfig{"west": {Endpoint: "http://west.example:4318"}}
	c.Skipped.Endpoint = "skipped.example:4317"
	return c
}

func TestClientsFindEveryDeclaredEndpoint(t *testing.T) {
	got := Clients(newClientsConfig())
	want := []Client{
		{Path: "delivery", Protocol: ProtocolGRPC, Endpoint: "collector.example:4317"},
		{Path: "control", Protocol: ProtocolGRPC, Endpoint: "control.example:443"},
		{Path: "upload", Protocol: ProtocolHTTP, Endpoint: "https://upload.example/v1/logs?tenant=lab"},
		{Path: "routes[0]::client", Protocol: ProtocolGRPC, Endpoint: "a.example:4317"},
		{Path: "peers::west", Protocol: ProtocolHTTP, Endpoint: "http://west.example:4318"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Clients:\n got %+v\nwant %+v", got, want)
	}
	if got := Clients(&struct{ Name string }{"x"}); got != nil {
		t.Fatalf("a configuration without clients has %+v", got)
	}
	if got := Clients(nil); got != nil {
		t.Fatalf("a nil configuration has %+v", got)
	}
}

func TestRedirectPointsACopyAtLoopbackSinks(t *testing.T) {
	eps := NewEndpoints(nil)
	t.Cleanup(func() { _ = eps.Close() })
	orig := newClientsConfig()
	id := component.MustNewID("livecapture")
	cfg, err := eps.Redirect(topology.KindExtension, id, orig)
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.(*clientsConfig)

	// The original is unchanged, including what the copy shares a path
	// with.
	if orig.Delivery.ClientConfig.Endpoint != "collector.example:4317" || orig.Delivery.ClientConfig.TLS.CAFile != "/etc/ca.pem" ||
		!orig.Delivery.ClientConfig.Auth.HasValue() || orig.Control.Endpoint != "control.example:443" ||
		orig.Upload.Get().ProxyURL != "http://proxy:3128" || orig.Routes[0].Client.Endpoint != "a.example:4317" ||
		orig.Peers["west"].Endpoint != "http://west.example:4318" {
		t.Fatalf("Redirect changed the original configuration: %+v", orig)
	}

	d := c.Delivery.ClientConfig
	if !strings.HasPrefix(d.Endpoint, "127.0.0.1:") || !d.TLS.Insecure || d.TLS.CAFile != "" || d.Auth.HasValue() {
		t.Fatalf("delivery is not a plain loopback client: %+v", d)
	}
	if len(d.Headers) != 1 || d.Headers[0].Name != "x-tenant" {
		t.Fatalf("delivery headers = %v, want them kept", d.Headers)
	}
	if !strings.HasPrefix(c.Control.Endpoint, "127.0.0.1:") || c.Control == orig.Control {
		t.Fatalf("control = %+v, want a loopback copy", c.Control)
	}
	up := c.Upload.Get()
	if !strings.HasPrefix(up.Endpoint, "http://127.0.0.1:") || !strings.HasSuffix(up.Endpoint, "/v1/logs?tenant=lab") || up.ProxyURL != "" {
		t.Fatalf("upload = %+v, want loopback with the path and query kept and no proxy", up)
	}
	if !strings.HasPrefix(c.Routes[0].Client.Endpoint, "127.0.0.1:") || c.Routes[1].Client.Endpoint != "" {
		t.Fatalf("routes = %+v", c.Routes)
	}
	if !strings.HasPrefix(c.Peers["west"].Endpoint, "http://127.0.0.1:") {
		t.Fatalf("peers = %+v", c.Peers)
	}
	if c.Skipped.Endpoint != "skipped.example:4317" || c.Name != "lab" {
		t.Fatalf("fields the configuration skips changed: %+v", c)
	}

	// A second instance shares the sinks.
	again, err := eps.Redirect(topology.KindExtension, id, orig)
	if err != nil {
		t.Fatal(err)
	}
	if again.(*clientsConfig).Delivery.ClientConfig.Endpoint != d.Endpoint {
		t.Fatal("a second Redirect of one component started another sink")
	}
	sim := eps.Simulated()
	if len(sim) != 5 || sim[0].Component != "extension/livecapture" || sim[0].Path != "control" || sim[0].Endpoint != "control.example:443" {
		t.Fatalf("Simulated = %+v", sim)
	}

	if err := eps.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := eps.Redirect(topology.KindExtension, id, orig); err == nil {
		t.Fatal("Redirect after Close succeeded")
	}
	plain := &struct{ Name string }{"x"}
	if got, err := eps.Redirect(topology.KindExtension, id, plain); err != nil || got != plain {
		t.Fatalf("a configuration without clients = %v, %v; want it as is", got, err)
	}
}

type receivedPayloads struct {
	mu  sync.Mutex
	got map[string][]telemetry.Payload
}

func (r *receivedPayloads) EndpointReceived(e EndpointRef, p telemetry.Payload) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got[e.Ref()+":"+e.Path] = append(r.got[e.Ref()+":"+e.Path], p)
}

func TestSinksServeOTLPExportAndRefuseTheRest(t *testing.T) {
	host := &receivedPayloads{got: map[string][]telemetry.Payload{}}
	eps := NewEndpoints(host)
	t.Cleanup(func() { _ = eps.Close() })
	cfg, err := eps.Redirect(topology.KindProcessor, component.MustNewID("capture"), newClientsConfig())
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.(*clientsConfig)

	// gRPC: OTLP export is received, a control call is refused.
	conn, err := grpc.NewClient(c.Delivery.ClientConfig.Endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	td := ptrace.NewTraces()
	td.ResourceSpans().AppendEmpty().ScopeSpans().AppendEmpty().Spans().AppendEmpty().SetName("checkout")
	if _, err := ptraceotlp.NewGRPCClient(conn).Export(context.Background(), ptraceotlp.NewExportRequestFromTraces(td)); err != nil {
		t.Fatalf("OTLP export: %v", err)
	}
	err = conn.Invoke(context.Background(), "/livecapture.v1.Control/StartSession", &emptypb.Empty{}, &emptypb.Empty{})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("control call = %v, want Unimplemented", err)
	}

	// HTTP: an OTLP path is received, anything else is 404.
	ld := plog.NewLogs()
	ld.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords().AppendEmpty().Body().SetStr("uploaded")
	body, err := plogotlp.NewExportRequestFromLogs(ld).MarshalProto()
	if err != nil {
		t.Fatal(err)
	}
	up := c.Upload.Get().Endpoint
	resp, err := http.Post(up, "application/x-protobuf", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("OTLP/HTTP export status %d", resp.StatusCode)
	}
	base := strings.TrimSuffix(up, "/v1/logs?tenant=lab")
	resp, err = http.Get(base + "/api/sessions")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /api/sessions status %d, want 404", resp.StatusCode)
	}

	host.mu.Lock()
	traces, logs := host.got["processor/capture:delivery"], host.got["processor/capture:upload"]
	host.mu.Unlock()
	if len(traces) != 1 || traces[0].Counts().Items != 1 {
		t.Fatalf("delivery received %+v", traces)
	}
	if len(logs) != 1 || logs[0].Counts().Items != 1 {
		t.Fatalf("upload received %+v", logs)
	}

	refused := map[string][]RefusedCall{}
	for _, s := range eps.Simulated() {
		refused[s.Path] = s.Refused
	}
	if r := refused["delivery"]; len(r) != 1 || r[0].Call != "/livecapture.v1.Control/StartSession" || r[0].Count != 1 {
		t.Fatalf("delivery refused %+v", r)
	}
	if r := refused["upload"]; len(r) != 1 || r[0].Call != "GET /api/sessions" {
		t.Fatalf("upload refused %+v", r)
	}
	decl := eps.Declarations()
	if len(decl) != 1 || decl[0].Provider != EndpointsProvider || decl[0].Ref != "processor/capture" ||
		decl[0].Mode != topology.ModeDependencyMocked || len(decl[0].Supplied) != 5 || len(decl[0].Warnings) != 2 {
		t.Fatalf("Declarations = %+v", decl)
	}
}
