package builtin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
)

// kubeResource is a Kubernetes resource type the fake API server serves:
// the kinds the k8sattributes processor watches (v1.1.0).
type kubeResource struct {
	Kind, Group, Version, Resource string
	Namespaced                     bool
}

func (r kubeResource) apiVersion() string {
	if r.Group == "" {
		return r.Version
	}
	return r.Group + "/" + r.Version
}

var kubeResources = []kubeResource{
	{"Pod", "", "v1", "pods", true},
	{"Namespace", "", "v1", "namespaces", false},
	{"Node", "", "v1", "nodes", false},
	{"ReplicaSet", "apps", "v1", "replicasets", true},
	{"Deployment", "apps", "v1", "deployments", true},
	{"StatefulSet", "apps", "v1", "statefulsets", true},
	{"DaemonSet", "apps", "v1", "daemonsets", true},
	{"Job", "batch", "v1", "jobs", true},
	{"CronJob", "batch", "v1", "cronjobs", true},
}

func kubeResourceOf(kind string) (kubeResource, bool) {
	for _, r := range kubeResources {
		if r.Kind == kind {
			return r, true
		}
	}
	return kubeResource{}, false
}

// kubeObject is one served object.
type kubeObject struct {
	res       kubeResource
	name      string
	namespace string
	raw       map[string]any
}

func (o kubeObject) String() string {
	if o.namespace == "" {
		return o.res.Kind + " " + o.name
	}
	return o.res.Kind + " " + o.namespace + "/" + o.name
}

func (o kubeObject) metadata() map[string]any {
	m, _ := o.raw["metadata"].(map[string]any)
	return m
}

// fields returns the object's selectable fields. Kubernetes supports more
// per kind; these are the ones the processor's filters use.
func (o kubeObject) fields() fields.Set {
	set := fields.Set{"metadata.name": o.name, "metadata.namespace": o.namespace}
	if spec, ok := o.raw["spec"].(map[string]any); ok {
		if n, ok := spec["nodeName"].(string); ok {
			set["spec.nodeName"] = n
		}
	}
	if status, ok := o.raw["status"].(map[string]any); ok {
		if p, ok := status["phase"].(string); ok {
			set["status.phase"] = p
		}
	}
	return set
}

func (o kubeObject) labels() labels.Set {
	set := labels.Set{}
	if l, ok := o.metadata()["labels"].(map[string]any); ok {
		for k, v := range l {
			set[k] = fmt.Sprint(v)
		}
	}
	return set
}

// kubeResourceVersion is the resource version of everything served: the
// objects never change during a run.
const kubeResourceVersion = "1"

// fakeKubeAPI is an in-process Kubernetes API server over TLS that serves a
// fixed set of objects. It implements what client-go informers use: list,
// watch, watch-list (sendInitialEvents with the initial-events-end
// bookmark) and metadata-only (PartialObjectMetadata) responses, with label
// and field selectors. Watches stay open with no further events.
type fakeKubeAPI struct {
	srv     *httptest.Server
	objects []kubeObject
	done    chan struct{}

	mu        sync.Mutex
	requested map[string]bool // kinds
	inflight  int
	last      time.Time
	unserved  []string
	// watchLists counts watch-list requests (sendInitialEvents).
	watchLists int
}

func newFakeKubeAPI(objects []kubeObject) *fakeKubeAPI {
	a := &fakeKubeAPI{objects: objects, done: make(chan struct{}), requested: map[string]bool{}}
	a.srv = httptest.NewUnstartedServer(a)
	a.srv.StartTLS()
	return a
}

// hostPort returns the server's loopback host and port.
func (a *fakeKubeAPI) hostPort() (string, string) {
	addr := strings.TrimPrefix(a.srv.URL, "https://")
	i := strings.LastIndex(addr, ":")
	return addr[:i], addr[i+1:]
}

func (a *fakeKubeAPI) close() {
	close(a.done)
	a.srv.Close()
}

// quiet reports whether no request is being answered and the server has
// been idle for d, counting from since at the earliest.
func (a *fakeKubeAPI) quiet(since time.Time, d time.Duration) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	last := a.last
	if since.After(last) {
		last = since
	}
	return a.inflight == 0 && time.Since(last) >= d
}

func (a *fakeKubeAPI) begin() {
	a.mu.Lock()
	a.inflight++
	a.last = time.Now()
	a.mu.Unlock()
}

func (a *fakeKubeAPI) end() {
	a.mu.Lock()
	a.inflight--
	a.last = time.Now()
	a.mu.Unlock()
}

func (a *fakeKubeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.begin()
	ended := false
	finish := func() {
		if !ended {
			ended = true
			a.end()
		}
	}
	defer finish()

	res, ns, ok := a.route(r.URL.Path)
	if !ok || r.Method != http.MethodGet {
		a.mu.Lock()
		a.unserved = append(a.unserved, r.Method+" "+r.URL.Path)
		a.mu.Unlock()
		writeStatus(w, http.StatusNotFound, "NotFound", "the debugger's mocked Kubernetes API does not serve "+r.URL.Path)
		return
	}
	a.mu.Lock()
	a.requested[res.Kind] = true
	a.mu.Unlock()

	q := r.URL.Query()
	ls, err := labels.Parse(q.Get("labelSelector"))
	if err != nil {
		writeStatus(w, http.StatusBadRequest, "BadRequest", err.Error())
		return
	}
	fs, err := fields.ParseSelector(q.Get("fieldSelector"))
	if err != nil {
		writeStatus(w, http.StatusBadRequest, "BadRequest", err.Error())
		return
	}
	partial := strings.Contains(r.Header.Get("Accept"), "as=PartialObjectMetadata")
	var items []any
	for _, o := range a.objects {
		if o.res != res || (ns != "" && o.namespace != ns) || !ls.Matches(o.labels()) || !fs.Matches(o.fields()) {
			continue
		}
		items = append(items, wireObject(o, partial))
	}

	w.Header().Set("Content-Type", "application/json")
	if q.Get("watch") != "true" && q.Get("watch") != "1" {
		kind, apiVersion := res.Kind+"List", res.apiVersion()
		if partial {
			kind, apiVersion = "PartialObjectMetadataList", "meta.k8s.io/v1"
		}
		if items == nil {
			items = []any{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"kind": kind, "apiVersion": apiVersion,
			"metadata": map[string]any{"resourceVersion": kubeResourceVersion},
			"items":    items,
		})
		return
	}

	flusher, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)
	if q.Get("sendInitialEvents") == "true" {
		a.mu.Lock()
		a.watchLists++
		a.mu.Unlock()
		for _, it := range items {
			_ = enc.Encode(map[string]any{"type": "ADDED", "object": it})
		}
		kind, apiVersion := res.Kind, res.apiVersion()
		if partial {
			kind, apiVersion = "PartialObjectMetadata", "meta.k8s.io/v1"
		}
		_ = enc.Encode(map[string]any{"type": "BOOKMARK", "object": map[string]any{
			"kind": kind, "apiVersion": apiVersion,
			"metadata": map[string]any{
				"resourceVersion": kubeResourceVersion,
				"annotations":     map[string]any{"k8s.io/initial-events-end": "true"},
			},
		}})
	}
	if flusher != nil {
		flusher.Flush()
	}
	finish()
	select {
	case <-r.Context().Done():
	case <-a.done:
	}
}

// route parses /api/v1/[namespaces/<ns>/]<resource> and
// /apis/<group>/<version>/[namespaces/<ns>/]<resource>.
func (a *fakeKubeAPI) route(path string) (kubeResource, string, bool) {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	var group, version string
	switch {
	case len(segs) >= 3 && segs[0] == "api":
		group, version, segs = "", segs[1], segs[2:]
	case len(segs) >= 4 && segs[0] == "apis":
		group, version, segs = segs[1], segs[2], segs[3:]
	default:
		return kubeResource{}, "", false
	}
	var ns, resource string
	switch {
	case len(segs) == 1:
		resource = segs[0]
	case len(segs) == 3 && segs[0] == "namespaces":
		ns, resource = segs[1], segs[2]
	default:
		return kubeResource{}, "", false
	}
	for _, r := range kubeResources {
		if r.Group == group && r.Version == version && r.Resource == resource && (ns == "" || r.Namespaced) {
			return r, ns, true
		}
	}
	return kubeResource{}, "", false
}

// served returns the objects of the kinds that were requested, and the
// kinds requested.
func (a *fakeKubeAPI) served() (objects []string, kinds []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, o := range a.objects {
		if a.requested[o.res.Kind] {
			objects = append(objects, o.String())
		}
	}
	for k := range a.requested {
		kinds = append(kinds, k)
	}
	sort.Strings(objects)
	sort.Strings(kinds)
	return objects, kinds
}

func wireObject(o kubeObject, partial bool) map[string]any {
	if partial {
		return map[string]any{"kind": "PartialObjectMetadata", "apiVersion": "meta.k8s.io/v1", "metadata": o.metadata()}
	}
	return o.raw
}

func writeStatus(w http.ResponseWriter, code int, reason, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"kind": "Status", "apiVersion": "v1", "metadata": map[string]any{},
		"status": "Failure", "message": msg, "reason": reason, "code": code,
	})
}
