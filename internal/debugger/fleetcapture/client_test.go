package fleetcapture

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewRefusesUnsafeConfiguration(t *testing.T) {
	for _, c := range []Config{
		{Endpoint: "http://fleet.example:8080", Token: "tok3n"},
		{Endpoint: "https://fleet.example/api", Token: "tok3n"},
		{Endpoint: "https://user@fleet.example", Token: "tok3n"},
		{Endpoint: "ftp://fleet.example", Token: "tok3n"},
		{Endpoint: "https://fleet.example", Token: ""},
		{Endpoint: "https://fleet.example", Token: "tok 3n"},
		{Endpoint: "https://fleet.example", Token: "tok3n", PluginID: "a/b"},
	} {
		if _, err := New(c); err == nil {
			t.Errorf("accepted %+v", c)
		} else if strings.Contains(err.Error(), c.Token) && c.Token != "" {
			t.Errorf("error names the token: %v", err)
		}
	}
	for _, c := range []Config{
		{Endpoint: "http://127.0.0.1:8080", Token: "t"},
		{Endpoint: "http://localhost:8080/", Token: "t"},
		{Endpoint: "http://fleet.internal:8080", Token: "t", AllowHTTP: true},
		{Endpoint: "https://fleet.example", Token: " t\n"},
	} {
		if _, err := New(c); err != nil {
			t.Errorf("refused %+v: %v", c, err)
		}
	}
}

// fakeFleet records requests and answers from a route table.
type fakeFleet struct {
	t      *testing.T
	routes map[string]func(w http.ResponseWriter, r *http.Request, body []byte)
}

func (f *fakeFleet) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer secret" {
		w.WriteHeader(401)
		return
	}
	body, _ := io.ReadAll(r.Body)
	route := f.routes[r.Method+" "+r.URL.Path]
	if route == nil {
		f.t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		w.WriteHeader(404)
		return
	}
	route(w, r, body)
}

func newFake(t *testing.T, routes map[string]func(http.ResponseWriter, *http.Request, []byte)) *Client {
	server := httptest.NewServer(&fakeFleet{t: t, routes: routes})
	t.Cleanup(server.Close)
	c, err := New(Config{Endpoint: server.URL, Token: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

const prefix = "/api/v1/plugins/livecapture/api/"

func TestCaptureLifecycleRequests(t *testing.T) {
	id := strings.Repeat("c", 32)
	op := Operation{ID: id, State: "starting", Revision: 1, Targets: []TargetOutcome{{PeerID: strings.Repeat("b", 64), Generation: 2, State: "pending"}}}
	c := newFake(t, map[string]func(http.ResponseWriter, *http.Request, []byte){
		"GET " + prefix + "v1/peers": func(w http.ResponseWriter, r *http.Request, _ []byte) {
			if r.URL.Query().Get("limit") != "64" || r.URL.Query().Get("cursor") != "next 1" {
				t.Errorf("peer query %s", r.URL.RawQuery)
			}
			reply(w, 200, PeerPage{Items: []Peer{{ID: "p"}}})
		},
		"POST " + prefix + "v1/channels": func(w http.ResponseWriter, r *http.Request, body []byte) {
			var req channelRequest
			if json.Unmarshal(body, &req) != nil || req.ExpiresAt.IsZero() || r.Header.Get("Content-Type") != "application/json" {
				t.Errorf("channel request %s", body)
			}
			reply(w, 201, Channel{ID: "ch", HeartbeatSeconds: 5})
		},
		"POST " + prefix + "v1/captures": func(w http.ResponseWriter, _ *http.Request, body []byte) {
			var req CaptureRequest
			if json.Unmarshal(body, &req) != nil || req.GroupID != "g" || req.ChannelID != "ch" || req.Targets != nil {
				t.Errorf("capture request %s", body)
			}
			reply(w, 202, op)
		},
		"GET " + prefix + "v1/captures/" + id: func(w http.ResponseWriter, _ *http.Request, _ []byte) {
			reply(w, 200, op)
		},
		"POST " + prefix + "v1/captures/" + id + "/stop": func(w http.ResponseWriter, r *http.Request, _ []byte) {
			if r.Header.Get("If-Match") != `"1"` {
				reply(w, 412, map[string]any{"error": map[string]string{"code": "capture_request_refused", "message": "revision mismatch"}})
				return
			}
			stopped := op
			stopped.State, stopped.Revision = "stopping", 2
			reply(w, 202, stopped)
		},
		"DELETE " + prefix + "v1/channels/ch": func(w http.ResponseWriter, _ *http.Request, _ []byte) {
			w.WriteHeader(204)
		},
	})
	ctx := context.Background()
	page, err := c.Peers(ctx, "next 1", 64)
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("peers %v %v", page, err)
	}
	ch, err := c.CreateChannel(ctx, time.Now().Add(time.Minute))
	if err != nil || ch.ID != "ch" {
		t.Fatalf("channel %v %v", ch, err)
	}
	created, err := c.CreateCapture(ctx, CaptureRequest{ID: id, ChannelID: "ch", GroupID: "g", CapturePoints: []Point{{"traces", "egress"}}, Signals: []string{"traces"}})
	if err != nil || created.State != "starting" || created.Terminal() {
		t.Fatalf("create %v %v", created, err)
	}
	if _, err := c.StopCapture(ctx, id, 7); StatusOf(err) != 412 {
		t.Fatalf("stale stop %v", err)
	}
	var refused *Error
	if _, err := c.StopCapture(ctx, id, 7); !errors.As(err, &refused) || refused.Message != "revision mismatch" {
		t.Fatalf("refusal not decoded: %v", err)
	}
	stopped, err := c.StopCapture(ctx, id, 1)
	if err != nil || stopped.Revision != 2 {
		t.Fatalf("stop %v %v", stopped, err)
	}
	if err := c.DeleteChannel(ctx, "ch"); err != nil {
		t.Fatal(err)
	}
}

func TestDeliveryRequests(t *testing.T) {
	batch := strings.Repeat("d", 32)
	waiting := true
	c := newFake(t, map[string]func(http.ResponseWriter, *http.Request, []byte){
		"GET " + prefix + "v1/channels/ch/next": func(w http.ResponseWriter, _ *http.Request, _ []byte) {
			if waiting {
				waiting = false
				w.WriteHeader(204)
				return
			}
			reply(w, 200, Envelope{BatchID: batch, Signal: "logs", Records: 3, Encoding: "otlp-protobuf", Data: []byte{0x0a, 0x00}})
		},
		"POST " + prefix + "v1/channels/ch/acks": func(w http.ResponseWriter, _ *http.Request, body []byte) {
			var ack Ack
			if json.Unmarshal(body, &ack) != nil || ack.BatchID != batch || ack.Accepted != 2 {
				t.Errorf("ack %s", body)
			}
			w.WriteHeader(204)
		},
	})
	ctx := context.Background()
	if e, err := c.Next(ctx, "ch"); e != nil || err != nil {
		t.Fatalf("empty channel %v %v", e, err)
	}
	e, err := c.Next(ctx, "ch")
	if err != nil || e.BatchID != batch || len(e.Data) != 2 {
		t.Fatalf("batch %v %v", e, err)
	}
	if err := c.Acknowledge(ctx, "ch", Ack{BatchID: batch, Accepted: 2}); err != nil {
		t.Fatal(err)
	}
}

func TestCoreReads(t *testing.T) {
	group := strings.Repeat("e", 32)
	c := newFake(t, map[string]func(http.ResponseWriter, *http.Request, []byte){
		"GET /api/v1/groups": func(w http.ResponseWriter, r *http.Request, _ []byte) {
			if r.URL.Query().Get("after") != group || r.URL.Query().Get("limit") != "10" {
				t.Errorf("group query %s", r.URL.RawQuery)
			}
			reply(w, 200, GroupPage{Items: []Group{{ID: group, Name: "edge", Kind: "selector", Revision: 1, Selector: "k8s.namespace.name=shop"}}})
		},
		"GET /api/v1/groups/" + group: func(w http.ResponseWriter, _ *http.Request, _ []byte) {
			reply(w, 200, Group{ID: group, Kind: "static", AgentIDs: []string{"a"}})
		},
		"GET /api/v1/groups/" + group + "/agents": func(w http.ResponseWriter, _ *http.Request, _ []byte) {
			reply(w, 200, AgentPage{Items: []Agent{{ID: "a", Connected: true}}})
		},
		"GET /api/v1/agents": func(w http.ResponseWriter, r *http.Request, _ []byte) {
			if r.URL.Query().Get("selector") != "k8s.namespace.name=shop" || r.URL.Query().Get("connected") != "true" {
				t.Errorf("agent query %s", r.URL.RawQuery)
			}
			// Fields the debugger does not read are ignored.
			_, _ = io.WriteString(w, `{"items":[{"id":"a","connected":true,"generation":"3","presence":"connected","attributes":{"host.name":"h"}}]}`)
		},
	})
	ctx := context.Background()
	if page, err := c.Groups(ctx, group, 10); err != nil || page.Items[0].Selector == "" {
		t.Fatalf("groups %v %v", page, err)
	}
	if g, err := c.Group(ctx, group); err != nil || len(g.AgentIDs) != 1 {
		t.Fatalf("group %v %v", g, err)
	}
	if page, err := c.GroupAgents(ctx, group, "", 0); err != nil || len(page.Items) != 1 {
		t.Fatalf("members %v %v", page, err)
	}
	if page, err := c.Agents(ctx, "k8s.namespace.name=shop", "", 0); err != nil || page.Items[0].Attributes["host.name"] != "h" {
		t.Fatalf("agents %v %v", page, err)
	}
}

func TestResponsesAreBounded(t *testing.T) {
	c := newFake(t, map[string]func(http.ResponseWriter, *http.Request, []byte){
		"GET " + prefix + "v1/peers": func(w http.ResponseWriter, _ *http.Request, _ []byte) {
			_, _ = io.WriteString(w, `{"items":[],"next_cursor":"`+strings.Repeat("x", responseBytes)+`"}`)
		},
		"GET " + prefix + "v1/captures/x": func(w http.ResponseWriter, _ *http.Request, _ []byte) {
			w.Header().Set("Location", "http://elsewhere.example/")
			w.WriteHeader(302)
		},
	})
	if _, err := c.Peers(context.Background(), "", 0); err == nil || !strings.Contains(err.Error(), "1 MiB") {
		t.Fatalf("oversize response %v", err)
	}
	if _, err := c.GetCapture(context.Background(), "x"); StatusOf(err) != 302 {
		t.Fatalf("redirect followed: %v", err)
	}
	if _, err := c.CreateCapture(context.Background(), CaptureRequest{Selector: strings.Repeat("s", requestBytes)}); err == nil {
		t.Fatal("oversize request sent")
	}
}
