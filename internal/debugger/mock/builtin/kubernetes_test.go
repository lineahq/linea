package builtin

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

func testObjects(t *testing.T) []kubeObject {
	t.Helper()
	objs, err := parseKubeObjects([]map[string]any{
		{"apiVersion": "v1", "kind": "List", "items": []any{
			map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{
				"name": "checkout-1", "namespace": "shop", "labels": map[string]any{"app": "checkout"},
			}, "spec": map[string]any{"nodeName": "worker-2"}, "status": map[string]any{"podIP": "10.42.0.17"}},
			map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{
				"name": "cart-1", "namespace": "shop", "labels": map[string]any{"app": "cart"},
			}, "spec": map[string]any{"nodeName": "worker-1"}},
		}},
		{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": "dns-1", "namespace": "kube-system"}},
		{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "shop", "labels": map[string]any{"team": "payments"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return objs
}

func restConfig(api *fakeKubeAPI) *rest.Config {
	return &rest.Config{Host: api.srv.URL, TLSClientConfig: rest.TLSClientConfig{Insecure: true}}
}

// TestFakeKubeAPIServesClients checks the fake server against client-go
// itself: typed lists with label and field selectors, metadata-only lists,
// and informers, which use watch-list by default in client-go v0.37.
func TestFakeKubeAPIServesClients(t *testing.T) {
	api := newFakeKubeAPI(testObjects(t))
	defer api.close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	kc, err := kubernetes.NewForConfig(restConfig(api))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, ns, labels, fields string
		want                     []string
	}{
		{"all", "", "", "", []string{"checkout-1", "cart-1", "dns-1"}},
		{"namespace", "shop", "", "", []string{"checkout-1", "cart-1"}},
		{"label", "", "app=checkout", "", []string{"checkout-1"}},
		{"node field", "", "", "spec.nodeName=worker-1", []string{"cart-1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			list, err := kc.CoreV1().Pods(tc.ns).List(ctx, metav1.ListOptions{LabelSelector: tc.labels, FieldSelector: tc.fields})
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, p := range list.Items {
				got = append(got, p.Name)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("pods %v, want %v", got, tc.want)
			}
		})
	}

	mc, err := metadata.NewForConfig(restConfig(api))
	if err != nil {
		t.Fatal(err)
	}
	nsGVR := schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	nss, err := mc.Resource(nsGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(nss.Items) != 1 || nss.Items[0].Labels["team"] != "payments" {
		t.Fatalf("namespaces %+v, want shop with its labels", nss.Items)
	}

	pods := cache.NewSharedInformer(cache.NewListWatchFromClient(kc.CoreV1().RESTClient(), "pods", "shop", fields.Everything()), &corev1.Pod{}, 0)
	namespaces := cache.NewSharedInformer(&cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, o metav1.ListOptions) (runtime.Object, error) {
			return mc.Resource(nsGVR).List(ctx, o)
		},
		WatchFuncWithContext: func(ctx context.Context, o metav1.ListOptions) (watch.Interface, error) {
			return mc.Resource(nsGVR).Watch(ctx, o)
		},
	}, &metav1.PartialObjectMetadata{}, 0)
	go pods.Run(ctx.Done())
	go namespaces.Run(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), pods.HasSynced, namespaces.HasSynced) {
		t.Fatal("informers did not sync")
	}
	if n := len(pods.GetStore().List()); n != 2 {
		t.Errorf("pod informer holds %d pods, want 2", n)
	}
	if n := len(namespaces.GetStore().List()); n != 1 {
		t.Errorf("namespace informer holds %d namespaces, want 1", n)
	}
	if api.watchLists == 0 {
		t.Error("the informers synced without a watch-list request")
	}
	objects, kinds := api.served()
	if strings.Join(kinds, ",") != "Namespace,Pod" || len(objects) != 4 {
		t.Errorf("served %v of kinds %v", objects, kinds)
	}
}

func TestParseKubeObjectsRejectsInvalidObjects(t *testing.T) {
	pod := func(md map[string]any) map[string]any {
		return map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": md}
	}
	for _, tc := range []struct {
		name    string
		objects []map[string]any
		want    string
	}{
		{"kind", []map[string]any{{"apiVersion": "v1", "kind": "Service", "metadata": map[string]any{"name": "a"}}}, `kind "Service" is not served`},
		{"api version", []map[string]any{{"apiVersion": "apps/v1beta1", "kind": "Deployment", "metadata": map[string]any{"name": "a"}}}, "want apps/v1"},
		{"metadata", []map[string]any{{"apiVersion": "v1", "kind": "Pod"}}, "has no metadata"},
		{"name", []map[string]any{pod(map[string]any{"namespace": "a"})}, "no metadata.name"},
		{"duplicate", []map[string]any{pod(map[string]any{"name": "a"}), pod(map[string]any{"name": "a", "namespace": "default"})}, "duplicate Pod default/a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseKubeObjects(tc.objects)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("parseKubeObjects = %v, want an error containing %q", err, tc.want)
			}
		})
	}
	objs, err := parseKubeObjects([]map[string]any{pod(map[string]any{"name": "a"})})
	if err != nil || objs[0].namespace != "default" || objs[0].metadata()["resourceVersion"] != kubeResourceVersion {
		t.Fatalf("parseKubeObjects = %+v, %v; want namespace default and a resource version", objs, err)
	}
}
