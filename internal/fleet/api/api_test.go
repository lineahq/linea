package api

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/fleet/auth"
	"github.com/lineahq/linea/internal/fleet/fleet"
)

func TestAPIAuthenticationAndContract(t *testing.T) {
	token := strings.Repeat("m", 32)
	credential, _ := auth.New(token)
	s := fleet.NewStore(3)
	id := "00000000000000000000000000000001"
	_, _ = s.Connect(id, fleet.Report{}, time.Now())
	h := New(s, credential, ServerInfo{ServerID: "test", Version: "test", Limits: Limits{MaxAgents: 3, MaxConnections: 3, MaxMessageBytes: 1024}})
	for _, tc := range []struct {
		method, path, token string
		want                int
	}{
		{"GET", "/healthz", "", 200}, {"GET", "/readyz", "", 503},
		{"GET", "/api/v1/server", "", 401}, {"GET", "/api/v1/agents", strings.Repeat("a", 32), 401},
		{"GET", "/api/v1/openapi.json", "", 401}, {"GET", "/api/v1/server", token, 200},
		{"GET", "/api/v1/agents", token, 200}, {"GET", "/api/v1/agents/" + id, token, 200},
		{"GET", "/api/v1/agents/00000000000000000000000000000002", token, 404},
		{"GET", "/api/v1/agents/bad", token, 400}, {"POST", "/api/v1/agents", token, 405},
		{"GET", "/api/v1/agents?limit=0", token, 400}, {"GET", "/api/v1/agents?limit=501", token, 400},
		{"GET", "/api/v1/agents?limit=1&limit=2", token, 400}, {"GET", "/api/v1/agents?unknown=1", token, 400},
		{"GET", "/api/v1/agents?connected=1", token, 400}, {"GET", "/api/v1/agents?after=bad", token, 400},
		{"GET", "/api/v1/agents?connected=false", token, 200}, {"GET", "/api/v1/agents?connected=%zz", token, 400},
		{"GET", "/api/v1/agents?after=", token, 400}, {"GET", "/api/v1/agents?connected=", token, 400},
		{"GET", "/api/v1/plugins", token, 200}, {"GET", "/api/v1/plugins/sample", token, 404},
		{"GET", "/api/v1/plugins?unexpected=true", token, 400}, {"POST", "/api/v1/plugins", token, 405},
		{"GET", "/api/v1/plugins/sample/status", token, 404}, {"GET", "/api/v1/openapi.json", token, 200},
	} {
		t.Run(tc.method+tc.path+tc.token[:min(len(tc.token), 1)], func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, nil)
			if tc.token != "" {
				r.Header.Set("Authorization", "Bearer "+tc.token)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.want, w.Body.String())
			}
			if !json.Valid(w.Body.Bytes()) || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("invalid JSON/cache contract")
			}
			if tc.want == 401 && w.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Fatal("missing challenge")
			}
			if tc.want == 405 && w.Header().Get("Allow") != "GET" {
				t.Fatal("missing Allow")
			}
		})
	}
	h.SetReady(true)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/readyz", nil))
	if w.Code != 200 {
		t.Fatal("not ready")
	}
	// Every documented route is present in the embedded contract.
	var spec struct {
		Paths map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(specification, &spec); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/agents", "/api/v1/credentials", "/api/v1/groups/{id}", "/api/v1/groups/{id}/agents", "/api/v1/events", "/metrics"} {
		if spec.Paths[path] == nil {
			t.Fatalf("missing public contract: %s", path)
		}
	}
}

func TestAgentInventoryQueries(t *testing.T) {
	token := strings.Repeat("m", 32)
	credential, _ := auth.New(token)
	s := fleet.NewStore(10)
	healthy, unhealthy := true, false
	for i, a := range []struct {
		service, namespace string
		healthy            *bool
	}{{"checkout", "shop", &healthy}, {"cart", "shop", &unhealthy}, {"edge", "ingress", nil}, {"cart", "staging", &healthy}} {
		id := strings.Repeat("0", 31) + string(rune('1'+i))
		_, _ = s.Connect(id, fleet.Report{Healthy: a.healthy, Attributes: map[string]string{"service.name": a.service, "k8s.namespace.name": a.namespace}}, time.Now())
	}
	h := New(s, credential, ServerInfo{})
	get := func(path string, want int) map[string]any {
		t.Helper()
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s: status %d, want %d: %s", path, w.Code, want, w.Body.String())
		}
		var body map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		return body
	}
	services := func(body map[string]any) string {
		names := []string{}
		for _, item := range body["items"].([]any) {
			names = append(names, item.(map[string]any)["attributes"].(map[string]any)["service.name"].(string))
		}
		return strings.Join(names, ",")
	}
	for path, want := range map[string]string{
		"/api/v1/agents?selector=k8s.namespace.name%3Dshop":                        "checkout,cart",
		"/api/v1/agents?selector=service.name+in+(cart,edge)":                      "cart,edge,cart",
		"/api/v1/agents?selector=service.name%3Dcart,k8s.namespace.name%3Dstaging": "cart",
		"/api/v1/agents?selector=team":                                             "",
		"/api/v1/agents?healthy=false":                                             "cart",
		"/api/v1/agents?sort=service.name":                                         "cart,cart,checkout,edge",
	} {
		if got := services(get(path, 200)); got != want {
			t.Fatalf("%s = %q, want %q", path, got, want)
		}
	}
	// Sorted pages continue from the opaque position.
	first := get("/api/v1/agents?sort=service.name&limit=3", 200)
	next := first["next_after"].(string)
	if got := services(get("/api/v1/agents?sort=service.name&limit=3&after="+next, 200)); got != "edge" {
		t.Fatalf("second sorted page = %q", got)
	}
	for _, path := range []string{
		"/api/v1/agents?selector=", "/api/v1/agents?selector=a+in+()", "/api/v1/agents?sort=team",
		"/api/v1/agents?sort=service.name&after=" + strings.Repeat("0", 32), "/api/v1/agents?healthy=yes",
		"/api/v1/summary?group_by=team", "/api/v1/summary?limit=1",
	} {
		get(path, 400)
	}
	summary := get("/api/v1/summary?group_by=service.name", 200)
	groups := summary["groups"].([]any)
	if top := groups[0].(map[string]any); top["value"] != "cart" || top["total"] != float64(2) || len(groups) != 3 {
		t.Fatalf("groups = %v", groups)
	}
	if _, ok := get("/api/v1/summary", 200)["groups"]; ok {
		t.Fatal("ungrouped summary has groups")
	}
}

// A refused request says which field broke which rule, never its value.
func TestInvalidRequestsNameTheRule(t *testing.T) {
	token := strings.Repeat("m", 32)
	credential, _ := auth.New(token)
	h := New(fleet.NewStore(10), credential, ServerInfo{})
	for path, want := range map[string]string{
		"/api/v1/agents?sort=team":              "invalid request: sort must be one of service.name,",
		"/api/v1/agents?limit=0":                "invalid request: limit must be 1 to 500",
		"/api/v1/agents?selector=a%3D%3Dsecret": "invalid request: selector term 1 must be",
		"/api/v1/agents?healthy=yes":            "invalid request: healthy must be true or false",
		"/api/v1/agents?secret=1":               "invalid request: query parameters are ",
	} {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var body struct {
			Error struct{ Code, Message string }
		}
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		if w.Code != 400 || body.Error.Code != "invalid_argument" || !strings.HasPrefix(body.Error.Message, want) || strings.Contains(body.Error.Message, "secret") {
			t.Fatalf("%s: %d %+v", path, w.Code, body.Error)
		}
	}
}
