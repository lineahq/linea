package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/lineahq/linea/internal/fleet/api"
	"github.com/lineahq/linea/internal/fleet/auth"
	"github.com/lineahq/linea/internal/fleet/fleet"
	"github.com/lineahq/linea/internal/fleet/gateway"
	"github.com/open-telemetry/opamp-go/protobufs"
	"google.golang.org/protobuf/proto"
)

// Run only against a disposable database supplied by scripts/fleet/test-postgres.ps1.
func TestPostgresLifecycle(t *testing.T) {
	url := os.Getenv("LINEA_FLEET_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("requires isolated PostgreSQL fixture")
	}
	ctx := context.Background()
	// Seed the unmodified S2 schema and a legacy bound credential, then exercise
	// the actual upgrade. Fresh installation is covered by the Docker smoke.
	legacy, err := auth.Generate(auth.CredentialRequest{ID: "00000000000000000000000000000009", Name: "legacy-bound", Role: "agent", ExpiresAt: new(time.Now().Add(time.Hour))}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	seed, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(ctx, migration); err != nil {
		t.Fatal("seed original schema:", err)
	}
	initialSum := sha256.Sum256([]byte(migration))
	if _, err := seed.Exec(ctx, "INSERT INTO fleet.schema_version(version,checksum) VALUES(1,$1)", hex.EncodeToString(initialSum[:])); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(ctx, "INSERT INTO fleet.credentials(id,name,role,agent_id,token_hash,created_at,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7)", legacy.Credential.ID, legacy.Credential.Name, legacy.Credential.Role, "09090909090909090909090909090909", legacy.Credential.Hash, legacy.Credential.CreatedAt, legacy.Credential.ExpiresAt); err != nil {
		t.Fatal(err)
	}
	_ = seed.Close(ctx)
	if old, err := Open(ctx, url, Options{ServerID: "integration-fleet", MaxAgents: 10, MaxEvents: 100}); !errors.Is(err, fleet.ErrConflict) {
		if old != nil {
			old.Close()
		}
		t.Fatal("server admitted unmigrated schema")
	}
	if err := Migrate(ctx, url); err != nil {
		t.Fatal("migration:", err)
	}
	if err := Migrate(ctx, url); err != nil {
		t.Fatal("repeat migration:", err)
	}
	issued, err := Bootstrap(ctx, url, "integration-fleet", "bootstrap-admin")
	if err != nil {
		t.Fatal("bootstrap:", err)
	}
	if issued.Credential.ExpiresAt != nil {
		t.Fatal("bootstrap administrator expires")
	}
	if _, err := Bootstrap(ctx, url, "integration-fleet", "another-admin"); !errors.Is(err, fleet.ErrConflict) {
		t.Fatal("bootstrap duplicated active administrator")
	}
	options := Options{ServerID: "integration-fleet", MaxAgents: 10, MaxEvents: 100, Heartbeat: 200 * time.Millisecond, Alive: time.Second, Flush: 200 * time.Millisecond}
	s, err := Open(ctx, url, options)
	if err != nil {
		t.Fatal("open:", err)
	}
	defer func() { s.Close() }()
	// Replicas share one database; migration refuses while any is alive.
	other, err := Open(ctx, url, options)
	if err != nil || other.ReplicaID() == s.ReplicaID() {
		t.Fatal("second replica was refused:", err)
	}
	pending := sha256.Sum256([]byte(credentialExpiryMigration))
	if _, err := s.pool.Exec(ctx, "DELETE FROM fleet.schema_version WHERE version=11"); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, url); !errors.Is(err, ErrReplicasActive) {
		t.Fatal("migration ran while replicas were alive:", err)
	}
	if _, err := s.pool.Exec(ctx, "INSERT INTO fleet.schema_version(version,checksum) VALUES(11,$1)", hex.EncodeToString(pending[:])); err != nil {
		t.Fatal(err)
	}
	if err := other.StopAgents(ctx); err != nil {
		t.Fatal("graceful replica stop:", err)
	}
	other.Close()
	service := &auth.Service{Repository: s}
	legacyRequest, _ := http.NewRequest("GET", "/v1/opamp", nil)
	legacyRequest.Header.Set("Authorization", "Bearer "+legacy.Token)
	legacyPrincipal, err := service.Authenticate(ctx, legacyRequest)
	if err != nil || legacyPrincipal.ID != legacy.Credential.ID || legacyPrincipal.Role != "agent" {
		t.Fatal("upgrade changed a legacy token")
	}
	// Previously bound tokens now enroll any UID, too.
	if generation, _, err := s.ConnectAgent(ctx, "08080808080808080808080808080808", legacyPrincipal.ID, fleet.Report{}, time.Now()); err != nil {
		t.Fatal("upgrade retained legacy UID binding")
	} else if err := s.DisconnectAgent(ctx, "08080808080808080808080808080808", generation, time.Now()); err != nil {
		t.Fatal(err)
	}
	aborted, abort := context.WithCancel(ctx)
	abort()
	_ = s.Ready(aborted)
	if err := s.Ready(ctx); err != nil {
		t.Fatal("cancelled readiness probe destroyed ownership")
	}
	newHTTP := func() *httptest.Server {
		h := api.NewAuthorized(s, &auth.Service{Repository: s}, api.ServerInfo{ServerID: options.ServerID, Storage: "postgres"}, s, &auth.Service{Repository: s})
		h.SetReady(true)
		server := httptest.NewServer(h)
		t.Cleanup(server.Close)
		return server
	}
	server := newHTTP()
	request := func(method, path, token, body, match string, want int) []byte {
		t.Helper()
		r, _ := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		if match != "" {
			r.Header.Set("If-Match", match)
		}
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != want {
			t.Fatalf("%s %s: status %d want %d (body omitted to protect secrets)", method, path, resp.StatusCode, want)
		}
		return data
	}
	admin := issued.Token
	issue := func(id, role string) auth.Issued {
		t.Helper()
		req := auth.CredentialRequest{ID: id, Name: role + "-fixture", Role: role, ExpiresAt: new(time.Now().Add(time.Hour))}
		body, _ := json.Marshal(req)
		data := request("POST", "/api/v1/credentials", admin, string(body), "", 201)
		var response auth.Issued
		if err := json.Unmarshal(data, &response); err != nil {
			t.Fatal(err)
		}
		if response.Token == "" || len(response.Credential.Hash) != 0 {
			t.Fatal("invalid one-time disclosure")
		}
		return response
	}
	reader := issue("00000000000000000000000000000011", "reader")
	if stored, err := s.Credential(ctx, issued.Credential.ID); err != nil || stored.ExpiresAt != nil || !stored.Active(time.Now()) {
		t.Fatal("stored bootstrap administrator has an expiry or is inactive")
	}
	request("POST", "/api/v1/credentials", admin, `{"id":"000000000000000000000000000000a1","name":"service","role":"reader"}`, "", 201)
	request("POST", "/api/v1/credentials", admin, `{"id":"000000000000000000000000000000a2","name":"agent","role":"agent"}`, "", 400)
	if raw, err := pgx.Connect(ctx, url); err != nil {
		t.Fatal(err)
	} else {
		_, err := raw.Exec(ctx, "INSERT INTO fleet.credentials(id,name,role,token_hash,created_at) VALUES($1,'raw-agent','agent',$2,now())", "000000000000000000000000000000a3", make([]byte, 32))
		_ = raw.Close(ctx)
		if err == nil {
			t.Fatal("database accepted an agent credential without expiry")
		}
	}
	lifecycle, err := s.SetPluginLifecycle(ctx, issued.Credential.ID, fleet.PluginLifecycle{ID: "inventory-example", Desired: "draining"}, 0)
	if err != nil || lifecycle.Revision != 1 {
		t.Fatal("plugin lifecycle create failed")
	}
	if _, err := s.SetPluginLifecycle(ctx, reader.Credential.ID, fleet.PluginLifecycle{ID: "inventory-example", Desired: "disabled"}, 1); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatal("reader changed durable lifecycle")
	}
	if _, err := s.SetPluginLifecycle(ctx, issued.Credential.ID, fleet.PluginLifecycle{ID: "inventory-example", Desired: "disabled"}, 0); !errors.Is(err, fleet.ErrPrecondition) {
		t.Fatal("stale lifecycle revision was accepted")
	}
	if _, err := s.pool.Exec(ctx, "ALTER TABLE fleet.events ADD CONSTRAINT fixture_reject_lifecycle CHECK(kind <> 'plugin.disabled') NOT VALID"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetPluginLifecycle(ctx, issued.Credential.ID, fleet.PluginLifecycle{ID: "inventory-example", Desired: "disabled"}, 1); err == nil {
		t.Fatal("lifecycle escaped failed event transaction")
	}
	states, err := s.PluginLifecycles(ctx)
	if err != nil || len(states) != 1 || states[0].Desired != "draining" || states[0].Revision != 1 {
		t.Fatal("lifecycle transaction did not roll back")
	}
	if _, err := s.pool.Exec(ctx, "ALTER TABLE fleet.events DROP CONSTRAINT fixture_reject_lifecycle"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetPluginLifecycle(ctx, issued.Credential.ID, fleet.PluginLifecycle{ID: "inventory-example", Desired: "disabled"}, 1); err != nil {
		t.Fatal(err)
	}
	uid := bytes.Repeat([]byte{1}, 16)
	id := hex.EncodeToString(uid)
	agent := issue("00000000000000000000000000000012", "agent")
	obsolete, _ := json.Marshal(map[string]any{"id": randomID(), "name": "obsolete", "role": "agent", "agent_id": id, "expires_at": time.Now().Add(time.Hour)})
	request("POST", "/api/v1/credentials", admin, string(obsolete), "", 400)
	request("GET", "/api/v1/agents", "", "", "", 401)
	request("GET", "/api/v1/agents", agent.Token, "", "", 401)
	request("GET", "/api/v1/credentials", reader.Token, "", "", 403)
	request("POST", "/api/v1/credentials", reader.Token, "{}", "", 403)
	request("POST", "/api/v1/credentials", admin, `{"unexpected":true}`, "", 400)
	request("POST", "/api/v1/credentials", admin, `{} {}`, "", 400)
	request("POST", "/api/v1/credentials", admin, `{"name":"`+strings.Repeat("a", 66000)+`"}`, "", 413)
	request("DELETE", "/api/v1/credentials/"+issued.Credential.ID, admin, "", "", 409)
	metadata := request("GET", "/api/v1/credentials", admin, "", "", 200)
	if bytes.Contains(metadata, []byte("of1_")) || bytes.Contains(metadata, []byte("token_hash")) || bytes.Contains(metadata, []byte(`"agent_id"`)) {
		t.Fatal("credential metadata exposed secrets or obsolete UID binding")
	}
	duplicate, _ := json.Marshal(auth.CredentialRequest{ID: reader.Credential.ID, Name: "duplicate", Role: "reader", ExpiresAt: new(time.Now().Add(time.Hour))})
	request("POST", "/api/v1/credentials", admin, string(duplicate), "", 409)

	g, err := gateway.NewAuthorized(s, service, nil, 5, 4096)
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(g)
	t.Cleanup(func() { _ = g.Close(); gw.Close() })
	dial := func(token string) *websocket.Conn {
		t.Helper()
		c, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(gw.URL, "http")+"/v1/opamp", http.Header{"Authorization": {"Bearer " + token}})
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if err != nil {
			t.Fatal("agent dial:", err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	send := func(c *websocket.Conn, m *protobufs.AgentToServer, reply bool) {
		t.Helper()
		data, err := proto.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		_ = c.SetWriteDeadline(time.Now().Add(3 * time.Second))
		if err := c.WriteMessage(websocket.BinaryMessage, append([]byte{0}, data...)); err != nil {
			t.Fatal(err)
		}
		if !reply {
			return
		}
		_ = c.SetReadDeadline(time.Now().Add(4 * time.Second))
		_, body, err := c.ReadMessage()
		if err != nil {
			t.Fatal("report response:", err)
		}
		var response protobufs.ServerToAgent
		if len(body) == 0 || body[0] != 0 {
			t.Fatal("missing wire header")
		}
		if err := proto.Unmarshal(body[1:], &response); err != nil {
			t.Fatal(err)
		}
		if response.Command != nil || response.RemoteConfig != nil || response.ConnectionSettings != nil || response.PackagesAvailable != nil || response.Capabilities != 1 {
			t.Fatal("reporting emitted control commands")
		}
	}
	closed := func(c *websocket.Conn) {
		t.Helper()
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, _, err := c.ReadMessage()
		if err == nil {
			t.Fatal("refused peer remained open")
		}
		if e, ok := err.(interface{ Timeout() bool }); ok && e.Timeout() {
			t.Fatal("peer was not disconnected")
		}
	}
	refused := func(token string) {
		t.Helper()
		peer, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(gw.URL, "http")+"/v1/opamp", http.Header{"Authorization": {"Bearer " + token}})
		if peer != nil {
			_ = peer.Close()
		}
		if response != nil {
			defer response.Body.Close()
		}
		if err == nil || response == nil || response.StatusCode != 401 {
			t.Fatal("invalid or wrong-role token reached OpAMP")
		}
	}
	refused("")
	refused(admin)
	refused(reader.Token)
	// A single deployment bearer token enrolls independent, previously unknown
	// UIDs concurrently. No credential is created per Collector.
	deployment := issue("00000000000000000000000000000015", "agent")
	request("GET", "/api/v1/agents", deployment.Token, "", "", 401)
	request("POST", "/api/v1/credentials", deployment.Token, "{}", "", 401)
	deploymentUIDs := [][]byte{bytes.Repeat([]byte{3}, 16), bytes.Repeat([]byte{4}, 16)}
	deploymentPeers := []*websocket.Conn{dial(deployment.Token), dial(deployment.Token)}
	for i, peer := range deploymentPeers {
		send(peer, &protobufs.AgentToServer{InstanceUid: deploymentUIDs[i], SequenceNum: 1, Health: &protobufs.ComponentHealth{Healthy: true}}, true)
		if a, err := s.GetAgent(ctx, hex.EncodeToString(deploymentUIDs[i])); err != nil || !a.Connected {
			t.Fatal("deployment token did not discover Collector")
		}
	}
	// The newest connection for a UID wins and the older socket is closed.
	duplicatePeer := dial(deployment.Token)
	send(duplicatePeer, &protobufs.AgentToServer{InstanceUid: deploymentUIDs[0], SequenceNum: 1}, true)
	closed(deploymentPeers[0])
	if a, err := s.GetAgent(ctx, hex.EncodeToString(deploymentUIDs[0])); err != nil || !a.Connected || a.Generation != 2 {
		t.Fatal("duplicate UID did not take over:", err)
	}
	// A shared key still cannot change one connection's UID.
	changing := dial(deployment.Token)
	send(changing, &protobufs.AgentToServer{InstanceUid: bytes.Repeat([]byte{5}, 16), SequenceNum: 1}, true)
	send(changing, &protobufs.AgentToServer{InstanceUid: bytes.Repeat([]byte{6}, 16), SequenceNum: 2}, false)
	closed(changing)
	if _, err := s.GetAgent(ctx, strings.Repeat("06", 16)); !errors.Is(err, fleet.ErrNotFound) {
		t.Fatal("changed UID created an inventory entry")
	}
	request("DELETE", "/api/v1/credentials/"+deployment.Credential.ID, admin, "", "", 204)
	closed(duplicatePeer)
	for i, peer := range deploymentPeers {
		closed(peer)
		if _, _, err := s.ConnectAgent(ctx, hex.EncodeToString(deploymentUIDs[i]), deployment.Credential.ID, fleet.Report{}, time.Now()); !errors.Is(err, auth.ErrUnauthorized) {
			t.Fatal("revoked deployment token reconnected")
		}
		if err := s.ObserveAgent(ctx, hex.EncodeToString(deploymentUIDs[i]), deployment.Credential.ID, 1, fleet.Report{}, time.Now()); !errors.Is(err, auth.ErrUnauthorized) {
			t.Fatal("revoked deployment token published")
		}
	}
	request("GET", "/api/v1/agents", deployment.Token, "", "", 401)
	refused(deployment.Token)
	// Expiry must close every session sharing a deployment token, even idle ones.
	expiring := issue("00000000000000000000000000000016", "agent")
	expiringPeers := []*websocket.Conn{dial(expiring.Token), dial(expiring.Token)}
	for i, peer := range expiringPeers {
		send(peer, &protobufs.AgentToServer{InstanceUid: bytes.Repeat([]byte{byte(7 + i)}, 16), SequenceNum: 1}, true)
	}
	if _, err := s.pool.Exec(ctx, "UPDATE fleet.credentials SET expires_at=created_at+interval '1 microsecond' WHERE id=$1", expiring.Credential.ID); err != nil {
		t.Fatal(err)
	}
	for _, peer := range expiringPeers {
		closed(peer)
	}
	refused(expiring.Token)
	if _, _, err := s.ConnectAgent(ctx, strings.Repeat("07", 16), expiring.Credential.ID, fleet.Report{}, time.Now()); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatal("expired deployment token registered")
	}
	c := dial(agent.Token)
	send(c, &protobufs.AgentToServer{InstanceUid: uid, SequenceNum: 1, Capabilities: ^uint64(0), Health: &protobufs.ComponentHealth{Healthy: true, LastError: "sensitive-detail"}}, true)
	a, err := s.GetAgent(ctx, id)
	if err != nil || a.Capabilities != ^uint64(0) || a.Health == nil || !a.Health.Healthy {
		t.Fatal("durable report was not committed:", err)
	}
	healthTime := a.Health.ObservedAt
	send(c, &protobufs.AgentToServer{InstanceUid: uid, SequenceNum: 2, Capabilities: 0}, true)
	a, _ = s.GetAgent(ctx, id)
	if a.Capabilities != 0 || !a.Health.ObservedAt.Equal(healthTime) {
		t.Fatal("omitted health or capability removal failed")
	}
	history, err := s.Events(ctx, "", 500)
	if err != nil {
		t.Fatal(err)
	}
	send(c, &protobufs.AgentToServer{InstanceUid: uid, SequenceNum: 3, Capabilities: 0, Health: &protobufs.ComponentHealth{Healthy: true}}, true)
	unchanged, err := s.Events(ctx, history.NextCursor, 100)
	if err != nil || len(unchanged.Items) != 0 {
		t.Fatal("identical health created heartbeat history")
	}
	// Fault injection: history publication fails after the inventory UPDATE.
	// Both must roll back; no uncommitted observation can become visible.
	beforeFault, _ := s.GetAgent(ctx, id)
	if _, err := s.pool.Exec(ctx, "ALTER TABLE fleet.events ADD CONSTRAINT fixture_reject_update CHECK(kind <> 'agent.changed') NOT VALID"); err != nil {
		t.Fatal(err)
	}
	unhealthy := false
	if err := s.ObserveAgent(ctx, id, agent.Credential.ID, beforeFault.Generation, fleet.Report{Capabilities: 77, Healthy: &unhealthy}, time.Now()); err == nil {
		t.Fatal("failed history publication was accepted")
	}
	afterFault, _ := s.GetAgent(ctx, id)
	if afterFault.Capabilities != beforeFault.Capabilities || afterFault.Health.Healthy != beforeFault.Health.Healthy || !afterFault.LastReportAt.Equal(*beforeFault.LastReportAt) {
		t.Fatal("inventory escaped a rolled-back history transaction")
	}
	if _, err := s.pool.Exec(ctx, "ALTER TABLE fleet.events DROP CONSTRAINT fixture_reject_update"); err != nil {
		t.Fatal(err)
	}
	groupID := "00000000000000000000000000000021"
	body := `{"name":"production","agent_ids":["` + id + `","` + id + `"]}`
	request("PUT", "/api/v1/groups/"+groupID, reader.Token, body, `"0"`, 403)
	request("PUT", "/api/v1/groups/"+groupID, admin, body, "", 428)
	request("PUT", "/api/v1/groups/"+groupID, admin, body, `"0"`, 201)
	request("PUT", "/api/v1/groups/"+groupID, admin, body, `"0"`, 412)
	request("GET", "/api/v1/groups/"+groupID, reader.Token, "", "", 200)
	group, err := s.GetGroup(ctx, groupID)
	if err != nil || group.Revision != 1 || len(group.AgentIDs) != 1 {
		t.Fatal("group revision/membership failed")
	}
	badID := "00000000000000000000000000000022"
	request("PUT", "/api/v1/groups/"+badID, admin, `{"name":"invalid","agent_ids":["ffffffffffffffffffffffffffffffff"]}`, `"0"`, 400)
	if _, err := s.GetGroup(ctx, badID); !errors.Is(err, fleet.ErrNotFound) {
		t.Fatal("failed group write was partially published")
	}
	request("GET", "/api/v1/events?cursor=not-valid", reader.Token, "", "", 400)
	request("GET", "/api/v1/summary", reader.Token, "", "", 200)
	metrics := request("GET", "/metrics", reader.Token, "", "", 200)
	if bytes.Contains(metrics, []byte(id)) || !bytes.Contains(metrics, []byte("linea_fleet_agents")) {
		t.Fatal("metrics cardinality/exposure failed")
	}
	request("DELETE", "/api/v1/credentials/"+agent.Credential.ID, admin, "", "", 204)
	closed(c)
	request("DELETE", "/api/v1/credentials/"+agent.Credential.ID, admin, "", "", 204)
	if err := s.ObserveAgent(ctx, id, agent.Credential.ID, a.Generation, fleet.Report{}, time.Now()); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatal("revoked authority published a report")
	}
	request("GET", "/api/v1/server", agent.Token, "", "", 401)
	agent2 := issue("00000000000000000000000000000013", "agent")
	c2 := dial(agent2.Token)
	send(c2, &protobufs.AgentToServer{InstanceUid: uid, SequenceNum: 1, Capabilities: 5, Health: &protobufs.ComponentHealth{Healthy: true}}, true)
	a, _ = s.GetAgent(ctx, id)
	oldGeneration := a.Generation
	oldHealth := a.Health.ObservedAt
	oldEvents, _ := s.Events(ctx, "", 500)
	// Simulate a crash: close ownership without graceful gateway StopAgents.
	s.Close()
	next, err := Open(ctx, url, options)
	if err != nil {
		t.Fatal("restart:", err)
	}
	s = next
	states, err = s.PluginLifecycles(ctx)
	if err != nil || len(states) != 1 || states[0].Desired != "disabled" || states[0].Revision != 2 {
		t.Fatal("crash recovery lost plugin disable")
	}
	_ = g.Close()
	gw.Close()
	server.Close()
	server = newHTTP()
	// Presence follows the crashed owner's heartbeat; history survives.
	waitUntil(t, func() bool { a, err := s.GetAgent(ctx, id); return err == nil && !a.Connected })
	a, err = s.GetAgent(ctx, id)
	if err != nil || a.Presence != "disconnected" || a.DisconnectedAt == nil || a.Health == nil || !a.Health.ObservedAt.Equal(oldHealth) {
		t.Fatal("crash recovery lost history or invented current presence")
	}
	if _, err = s.Events(ctx, oldEvents.NextCursor, 100); err != nil {
		t.Fatal("event cursor did not survive restart:", err)
	}
	request("GET", "/api/v1/groups/"+groupID, reader.Token, "", "", 200)
	newGeneration, _, err := s.ConnectAgent(ctx, id, agent2.Credential.ID, fleet.Report{}, time.Now())
	if err != nil || newGeneration <= oldGeneration {
		t.Fatal("restart generation failed")
	}
	if err := s.DisconnectAgent(ctx, id, oldGeneration, time.Now()); err != nil {
		t.Fatal(err)
	}
	a, _ = s.GetAgent(ctx, id)
	if !a.Connected {
		t.Fatal("stale close overrode new generation")
	}
	// Exercise bounded retention without creating heartbeat history.
	before, _ := s.Events(ctx, "", 500)
	for revision := int64(1); revision <= 4; revision++ {
		if _, err := s.PutGroup(ctx, issued.Credential.ID, fleet.Group{ID: groupID, Name: "production", AgentIDs: []string{id}}, revision); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.trimEvents(ctx, 3); err != nil {
		t.Fatal("trim:", err)
	}
	if _, err := s.Events(ctx, before.NextCursor, 100); !errors.Is(err, fleet.ErrCursorExpired) {
		t.Fatal("retention failed to signal cursor gap")
	}
	retained, err := s.Events(ctx, "", 500)
	if err != nil || len(retained.Items) > 3 {
		t.Fatal("event retention exceeded bound")
	}
	// An expired credential is rejected even if its hash is otherwise valid.
	past := time.Now().Add(-time.Hour)
	expired, _ := auth.Generate(auth.CredentialRequest{ID: "00000000000000000000000000000014", Name: "expired", Role: "reader", ExpiresAt: new(past.Add(time.Minute))}, past)
	if err := s.InsertCredential(ctx, issued.Credential.ID, expired.Credential); err != nil {
		t.Fatal(err)
	}
	request("GET", "/api/v1/agents", expired.Token, "", "", 401)
	// A replica whose registration vanished fences itself.
	if _, err := s.pool.Exec(ctx, "DELETE FROM fleet.replicas WHERE id=$1", s.ReplicaID()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.Lost():
	case <-time.After(5 * time.Second):
		t.Fatal("reaped replica did not notice")
	}
	if err := s.Ready(ctx); !errors.Is(err, fleet.ErrUnavailable) {
		t.Fatal("lost replica was reported ready")
	}
	if _, _, err := s.ConnectAgent(ctx, id, agent2.Credential.ID, fleet.Report{}, time.Now()); !errors.Is(err, fleet.ErrUnavailable) {
		t.Fatal("publication continued after ownership loss")
	}
	request("GET", "/readyz", "", "", "", 503)
	request("GET", "/api/v1/agents", admin, "", "", 503)
	s.Close()
	wrong := options
	wrong.ServerID = "wrong-authority"
	if other, err := Open(ctx, url, wrong); !errors.Is(err, fleet.ErrConflict) {
		if other != nil {
			other.Close()
		}
		t.Fatal("database identity was silently reassigned")
	}
	maintenance, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer maintenance.Close(ctx)
	if _, err := maintenance.Exec(ctx, "UPDATE fleet.schema_version SET version=12 WHERE version=11"); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, url); !errors.Is(err, fleet.ErrConflict) {
		t.Fatal("migration accepted a newer schema")
	}
	if other, err := Open(ctx, url, options); !errors.Is(err, fleet.ErrConflict) {
		if other != nil {
			other.Close()
		}
		t.Fatal("server accepted a newer schema")
	}
	if _, err := maintenance.Exec(ctx, "UPDATE fleet.schema_version SET version=11,checksum='tampered' WHERE version=12"); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, url); !errors.Is(err, fleet.ErrConflict) {
		t.Fatal("migration accepted a modified checksum")
	}
	if _, err := maintenance.Exec(ctx, "UPDATE fleet.schema_version SET checksum=$1 WHERE version=11", hex.EncodeToString(pending[:])); err != nil {
		t.Fatal(err)
	}
	if _, err := maintenance.Exec(ctx, "UPDATE fleet.schema_version SET checksum='tampered' WHERE version=1"); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, url); !errors.Is(err, fleet.ErrConflict) {
		t.Fatal("migration ignored a changed historical checksum")
	}
	if _, err := maintenance.Exec(ctx, "UPDATE fleet.schema_version SET checksum=$1 WHERE version=1", hex.EncodeToString(initialSum[:])); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, url); err != nil {
		t.Fatal("known schema could not be reopened:", err)
	}
}

func waitUntil(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for replica state")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
