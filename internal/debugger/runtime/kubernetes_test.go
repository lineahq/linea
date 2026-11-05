package runtime

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"go.opentelemetry.io/collector/pdata/ptrace"

	"github.com/lineahq/linea/internal/debugger/mock"
	"github.com/lineahq/linea/internal/debugger/telemetry"
	"github.com/lineahq/linea/internal/debugger/topology"
)

var kubernetesConfig = filepath.Join("..", "..", "..", "testdata", "receivers", "kubernetes", "config.yaml")

func kubeObjects() []map[string]any {
	return []map[string]any{
		{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{
			"name": "checkout-7d9f8b6c4-x2x9q", "namespace": "shop", "uid": "0b7c3f52",
			"labels":          map[string]any{"app.kubernetes.io/component": "api", "pod-template-hash": "7d9f8b6c4"},
			"ownerReferences": []any{map[string]any{"apiVersion": "apps/v1", "kind": "ReplicaSet", "name": "checkout-7d9f8b6c4", "uid": "5e1d", "controller": true}},
		}, "spec": map[string]any{"nodeName": "worker-2"}, "status": map[string]any{"phase": "Running", "podIP": "10.42.0.17"}},
		{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": "cart-1", "namespace": "shop", "uid": "7a6b"},
			"spec": map[string]any{"nodeName": "worker-1"}, "status": map[string]any{"phase": "Running", "podIP": "10.42.0.23"}},
		{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "shop", "labels": map[string]any{"team": "payments"}}},
		{"apiVersion": "v1", "kind": "Node", "metadata": map[string]any{"name": "worker-2"}},
	}
}

// spanInput is one span whose resource carries attrs.
func spanInput(attrs map[string]string, clientAddress string) Input {
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	for k, v := range attrs {
		rs.Resource().Attributes().PutStr(k, v)
	}
	span := rs.ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	span.SetName("op")
	span.SetTraceID([16]byte{1})
	span.SetSpanID([8]byte{1})
	return Input{Payload: telemetry.FromTraces(td), ClientAddress: clientAddress}
}

// resourceAttrs returns the resource attributes of each exported resource,
// keyed by service.name.
func resourceAttrs(t *testing.T, r *runresult.Result) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, o := range r.Outputs {
		p, err := telemetry.Decode(o.Data)
		if err != nil {
			t.Fatal(err)
		}
		rss := p.Traces.ResourceSpans()
		for i := range rss.Len() {
			attrs := rss.At(i).Resource().Attributes().AsRaw()
			out[attrs["service.name"].(string)] = attrs
		}
	}
	return out
}

// TestKubernetesEnrichesFromMockedAPI runs the real k8sattributes processor
// with the unchanged config (auth_type serviceAccount) against the mocked
// API: one input is associated by its connection address, the other by its
// k8s.pod.ip resource attribute.
func TestKubernetesEnrichesFromMockedAPI(t *testing.T) {
	r := runCompleted(t, Options{
		ConfigPath: kubernetesConfig,
		Inputs: []Input{
			spanInput(map[string]string{"service.name": "checkout"}, "10.42.0.17"),
			spanInput(map[string]string{"service.name": "cart", "k8s.pod.ip": "10.42.0.23"}, ""),
		},
		Mocks: mock.Spec{Objects: kubeObjects()},
	})
	got := resourceAttrs(t, r)
	for svc, want := range map[string]map[string]string{
		"checkout": {"k8s.pod.name": "checkout-7d9f8b6c4-x2x9q", "k8s.namespace.name": "shop", "k8s.deployment.name": "checkout",
			"k8s.node.name": "worker-2", "app.component": "api", "team": "payments"},
		"cart": {"k8s.pod.name": "cart-1", "k8s.pod.uid": "7a6b", "team": "payments"},
	} {
		for k, v := range want {
			if got[svc][k] != v {
				t.Errorf("%s: %s = %v, want %s (attributes %v)", svc, k, got[svc][k], v, got[svc])
			}
		}
	}
	if r.Inputs[0].ClientAddress != "10.42.0.17" {
		t.Errorf("input client address %q", r.Inputs[0].ClientAddress)
	}

	if len(r.Mocks) != 1 {
		t.Fatalf("mocks %+v, want one declaration", r.Mocks)
	}
	d := r.Mocks[0]
	if d.Ref != "processor/k8sattributes" || d.Mode != topology.ModeDependencyMocked {
		t.Errorf("declaration %+v", d)
	}
	if want := []string{"Namespace shop", "Pod shop/cart-1", "Pod shop/checkout-7d9f8b6c4-x2x9q"}; !slices.Equal(d.Supplied, want) {
		t.Errorf("supplied %q, want %q", d.Supplied, want)
	}
	if !slices.ContainsFunc(d.Warnings, func(s string) bool { return strings.Contains(s, "not read") && strings.Contains(s, "Node worker-2") }) {
		t.Errorf("warnings %q do not name the unread node", d.Warnings)
	}
	if _, ok := os.LookupEnv("KUBERNETES_SERVICE_HOST"); ok {
		t.Error("KUBERNETES_SERVICE_HOST is still set after the run")
	}
}

// TestKubernetesWithoutObjectsWarns: the processor still runs against the
// (empty) mocked API, never a real cluster, and the run says so.
func TestKubernetesWithoutObjectsWarns(t *testing.T) {
	r := runCompleted(t, Options{
		ConfigPath: kubernetesConfig,
		Inputs:     []Input{spanInput(map[string]string{"service.name": "checkout"}, "10.42.0.17")},
	})
	if _, enriched := resourceAttrs(t, r)["checkout"]["k8s.pod.name"]; enriched {
		t.Error("enriched without objects")
	}
	if len(r.Mocks) != 1 || !slices.ContainsFunc(r.Mocks[0].Warnings, func(s string) bool { return strings.Contains(s, "no Kubernetes objects are mocked") }) {
		t.Fatalf("mocks %+v, want a no-objects warning", r.Mocks)
	}
}

// TestReferenceRunEnrichesFromMockedObjects: the reference Collector's
// processor queries the same mocked API, and the real OTLP receiver's
// loopback connection is reported as the input's declared client address.
func TestReferenceRunEnrichesFromMockedObjects(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := RunReference(ctx, Options{
		ConfigPath: kubernetesConfig,
		Inputs:     []Input{spanInput(map[string]string{"service.name": "checkout"}, "10.42.0.17")},
		Mocks:      mock.Spec{Objects: kubeObjects()},
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != runresult.StatusCompleted {
		t.Fatalf("status %s: %s", r.Status, r.StatusReason)
	}
	if got := resourceAttrs(t, r)["checkout"]["k8s.pod.name"]; got != "checkout-7d9f8b6c4-x2x9q" {
		t.Errorf("k8s.pod.name = %v, want the pod at the declared client address", got)
	}
	if len(r.Mocks) != 1 || r.Mocks[0].Ref != "processor/k8sattributes" {
		t.Errorf("mocks %+v, want the k8sattributes declaration", r.Mocks)
	}
	if _, ok := os.LookupEnv("KUBERNETES_SERVICE_HOST"); ok {
		t.Error("KUBERNETES_SERVICE_HOST is still set after the run")
	}
}

func TestParseClientAddress(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"10.42.0.17", "10.42.0.17:0"},
		{"10.42.0.17:5000", "10.42.0.17:5000"},
		{"[fd00::17]:5000", "[fd00::17]:5000"},
		{"fd00::17", "[fd00::17]:0"},
	} {
		a, err := ParseClientAddress(tc.in)
		if err != nil || a.String() != tc.want {
			t.Errorf("ParseClientAddress(%q) = %v, %v; want %s", tc.in, a, err, tc.want)
		}
	}
	if _, err := ParseClientAddress("checkout.shop"); err == nil {
		t.Error("a host name was accepted")
	}
}
