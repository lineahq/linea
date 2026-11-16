package gateway

import (
	"bytes"
	"context"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/fleet/auth"
	"github.com/lineahq/linea/internal/fleet/fleet"
	"github.com/open-telemetry/opamp-go/protobufs"
	"google.golang.org/protobuf/proto"
)

// byToken admits any bearer token and names the credential after it.
type byToken struct{}

func (byToken) Authenticate(_ context.Context, r *http.Request) (auth.Principal, error) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		return auth.Principal{}, auth.ErrUnauthorized
	}
	return auth.Principal{ID: token, Role: "agent"}, nil
}

func (byToken) Active(_ context.Context, ps []auth.Principal) (map[string]bool, error) {
	active := map[string]bool{}
	for _, p := range ps {
		active[p.ID] = true
	}
	return active, nil
}

func TestInstanceUIDBindsToCredential(t *testing.T) {
	cases := []struct {
		name string
		// second connects with this credential while the first is still
		// connected or after it closed.
		second          string
		firstDisconnect bool
		refused         bool
	}{
		{name: "same credential takes over its own connection", second: "alpha"},
		{name: "another credential is refused while connected", second: "bravo", refused: true},
		{name: "another credential takes over after a disconnect", second: "bravo", firstDisconnect: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := fleet.NewStore(4)
			g, err := NewAuthorized(store, byToken{}, nil, 4, 1024)
			if err != nil {
				t.Fatal(err)
			}
			h := httptest.NewServer(g)
			t.Cleanup(func() { g.Close(); h.Close() })
			url := "ws" + strings.TrimPrefix(h.URL, "http") + "/v1/opamp"
			uid := bytes.Repeat([]byte{9}, 16)
			id := hex.EncodeToString(uid)

			first := dial(t, url, "alpha")
			exchange(t, first, &protobufs.AgentToServer{InstanceUid: uid, SequenceNum: 1, Capabilities: 7})
			before, _ := store.Get(id)
			if before.CredentialID != "alpha" {
				t.Fatalf("credential = %q", before.CredentialID)
			}
			if tc.firstDisconnect {
				_ = first.Close()
				waitFor(t, func() bool { a, _ := store.Get(id); return !a.Connected })
			}

			second := dial(t, url, tc.second)
			send(t, second, &protobufs.AgentToServer{InstanceUid: uid, SequenceNum: 1, Capabilities: 7})
			_ = second.SetReadDeadline(time.Now().Add(3 * time.Second))
			_, frame, err := second.ReadMessage()
			if tc.refused {
				response := &protobufs.ServerToAgent{}
				if err != nil || len(frame) == 0 || proto.Unmarshal(frame[1:], response) != nil {
					t.Fatalf("no error response: %v", err)
				}
				if e := response.ErrorResponse; e == nil || e.Type != protobufs.ServerErrorResponseType_ServerErrorResponseType_BadRequest ||
					strings.Contains(e.ErrorMessage, "alpha") || strings.Contains(e.ErrorMessage, "bravo") {
					t.Fatalf("error response = %+v", response.ErrorResponse)
				}
				expectClosed(t, second)
				after, _ := store.Get(id)
				if after.Generation != before.Generation || after.CredentialID != "alpha" || !after.Connected {
					t.Fatalf("refusal changed the agent: %+v -> %+v", before, after)
				}
				// The holder keeps its session.
				exchange(t, first, &protobufs.AgentToServer{InstanceUid: uid, SequenceNum: 2, Capabilities: 7})
				if g.credentialRefused.Load() != 1 {
					t.Fatalf("refusals = %d", g.credentialRefused.Load())
				}
				var metrics strings.Builder
				g.WriteMetrics(&metrics)
				if !strings.Contains(metrics.String(), "linea_fleet_agent_credential_refused_total 1\n") {
					t.Fatal("refusal metric missing")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			after, _ := store.Get(id)
			if after.Generation != before.Generation+1 || after.CredentialID != tc.second || !after.Connected {
				t.Fatalf("takeover: %+v -> %+v", before, after)
			}
			if g.credentialRefused.Load() != 0 {
				t.Fatalf("refusals = %d", g.credentialRefused.Load())
			}
		})
	}
}
