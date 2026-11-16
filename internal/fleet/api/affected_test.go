package api

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/fleet/auth"
	"github.com/lineahq/linea/internal/fleet/configmgmt"
	"github.com/lineahq/linea/internal/fleet/fleet"
)

// groupControl stores one group so a change can be compared with it.
type groupControl struct {
	fakeControl
	group fleet.Group
}

func (c groupControl) GetGroup(_ context.Context, id string) (fleet.Group, error) {
	if id != c.group.ID {
		return fleet.Group{}, fleet.ErrNotFound
	}
	return c.group, nil
}

func TestGroupChangeAboveUnstagedLimitIsRefused(t *testing.T) {
	s := fleet.NewStore(8)
	credential := auth.Credential{ID: strings.Repeat("72", 16), Role: "agent", Labels: map[string]string{"env": "prod"}, RemoteConfig: true}
	for i := range 3 {
		id := strings.Repeat(string(rune('a'+i))+"1", 16)
		if _, _, err := s.ConnectAgent(context.Background(), id, credential.ID, fleet.Report{Capabilities: 3, Attributes: map[string]string{"service.name": "edge"}}, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	group := strings.Repeat("9a", 16)
	control := groupControl{group: fleet.Group{ID: group, Name: "g", Kind: fleet.GroupSelector, Selector: "service.name=core", Revision: 1}}
	target, err := configmgmt.NewTarget(configmgmt.Assignment{ID: strings.Repeat("e1", 16), Name: "base", ConfigID: strings.Repeat("c1", 16), Version: 1, Layer: 10,
		GroupID: group, CredentialLabels: map[string]string{"env": "prod"}}, bytes.Repeat([]byte{1}, 32), control.group.Selector, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := NewAuthorized(s, roles{}, ServerInfo{ServerID: "test"}, control, &auth.Service{Repository: &fakeCredentials{c: credential}})
	h.SetConfigManagement(&fakeConfigs{targets: []configmgmt.Target{target}}, nil)
	h.SetUnstagedLimit(2)

	path := "/api/v1/groups/" + group
	edge := map[string]any{"name": "g", "selector": "service.name=edge"}
	// Three connected agents would get the layer at once.
	if w := call(t, h, "PUT", path, "admin", edge); w.Code != 409 || errorCode(t, w) != "too_many_affected" || !strings.Contains(w.Body.String(), "max_affected=3") {
		t.Fatalf("unstaged change: %d %s", w.Code, w.Body)
	}
	if w := call(t, h, "PUT", path+"?max_affected=2", "admin", edge); w.Code != 409 {
		t.Fatalf("max_affected below the count: %d", w.Code)
	}
	if w := call(t, h, "PUT", path+"?max_affected=3", "admin", edge); w.Code != 200 {
		t.Fatalf("acknowledged change: %d %s", w.Code, w.Body)
	}
	for _, q := range []string{"?max_affected=-1", "?max_affected=x", "?other=1", "?max_affected=1&max_affected=2"} {
		if w := call(t, h, "PUT", path+q, "admin", edge); w.Code != 400 {
			t.Fatalf("%s: %d", q, w.Code)
		}
	}
	// A change no connected agent sees needs no acknowledgement, nor does a
	// static group whose members change nothing.
	if w := call(t, h, "PUT", path, "admin", map[string]any{"name": "g", "selector": "service.name=other"}); w.Code != 200 {
		t.Fatalf("change without effect: %d %s", w.Code, w.Body)
	}
	if w := call(t, h, "PUT", path, "admin", map[string]any{"name": "g", "agent_ids": []string{strings.Repeat("a1", 16), strings.Repeat("b1", 16)}}); w.Code != 200 {
		t.Fatalf("static change of two: %d %s", w.Code, w.Body)
	}
	if w := call(t, h, "PUT", path, "admin", map[string]any{"name": "g", "agent_ids": []string{strings.Repeat("a1", 16), strings.Repeat("b1", 16), strings.Repeat("c1", 16)}}); w.Code != 409 {
		t.Fatalf("static change of three: %d %s", w.Code, w.Body)
	}
	// Without config management nothing is counted.
	plain := NewAuthorized(s, roles{}, ServerInfo{ServerID: "test"}, control, nil)
	plain.SetUnstagedLimit(1)
	if w := call(t, plain, "PUT", path, "admin", edge); w.Code != 200 {
		t.Fatalf("reporting-only server: %d %s", w.Code, w.Body)
	}
}
