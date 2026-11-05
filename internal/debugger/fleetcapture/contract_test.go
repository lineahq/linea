package fleetcapture

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// testdata holds copies of fleet's published contracts: the livecapture
// plugin's openapi.json and core's internal/debugger/api/openapi.json. Refresh them
// when fleet changes either document.
func loadSchemas(t *testing.T, file string) map[string]any {
	t.Helper()
	data, err := os.ReadFile("testdata/" + file)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Components struct {
			Schemas map[string]any `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	return document.Components.Schemas
}

// The mirrors name exactly the plugin's fields: values set every optional
// field, so a key on either side alone is drift. Core agents are read in part,
// so only their encoded fields must exist.
func TestTypesMatchContracts(t *testing.T) {
	id, peer, now := strings.Repeat("a", 32), strings.Repeat("b", 64), time.Now().UTC()
	members := 2
	filter := Filter{Resource: []string{"true"}, Traces: &TraceFilter{Span: []string{"true"}}, Logs: &LogFilter{Log: []string{"true"}},
		Metrics: &MetricFilter{Metric: []string{"true"}, Datapoint: []string{"true"}}}
	plugin := loadSchemas(t, "livecapture-openapi.json")
	for name, value := range map[string]any{
		"Peer": Peer{AuthorityID: id, PluginInstance: id, ID: peer, Generation: 1, CredentialID: "c", AgentID: id, Identity: Identity{Cluster: "c", Namespace: "n", Pod: "p",
			CollectorGroup: "g", CollectorVersion: "v", DistributionVersion: "v", Enabled: true, CapturePoints: []CapturePoint{{"traces", "egress", "traces", "redacted"}},
			FilterLimits: &FilterLimits{MaxConditions: 16, MaxConditionChars: 256, MaxRegexChars: 256}}},
		"Channel":        Channel{AuthorityID: id, PluginInstance: id, ID: id, ExpiresAt: now, HeartbeatSeconds: 5},
		"ChannelRequest": channelRequest{ExpiresAt: now},
		"Envelope":       Envelope{AuthorityID: id, PluginInstance: id, PluginEpoch: id, BatchID: id, CaptureID: id, PeerID: peer, Generation: 1, Signal: "logs", Records: 1, Encoding: "otlp-protobuf", Data: []byte{1}, ObservedAt: now},
		"Ack":            Ack{BatchID: id, Accepted: 1},
		"CaptureRequest": CaptureRequest{AuthorityID: id, PluginInstance: id, ID: id, ChannelID: id, Targets: []Target{{peer, 1}}, Agents: []string{id}, Selector: "a", GroupID: id,
			CapturePoints: []Point{{"traces", "egress"}}, Signals: []string{"traces"}, Filter: filter, Sampling: &Sampling{Rate: 1, Mode: "hash"},
			Limits: Limits{BytesPerSecond: 1, RecordsPerSecond: 1, TotalBytes: 1, TotalRecords: 1, RecordBytes: 1}, ExpiresAt: now},
		"Operation": Operation{AuthorityID: id, ID: id, State: "active", Reason: "r", Revision: 1, ExpiresAt: now, Targets: []TargetOutcome{{PeerID: peer, Generation: 1, AgentID: id, State: "active",
			Stats: &TargetStats{Selected: 1, EnqueuedRecords: 1, EnqueuedBytes: 1, FilterErrors: 1, Dropped: map[string]int64{"sampling": 1}}}}, ActingFor: &Acting{ID: "user:a", Name: "A", RequestID: "r"}},
	} {
		match(t, plugin, name, plugin[name], encode(t, value), true)
	}
	core := loadSchemas(t, "fleet-openapi.json")
	match(t, core, "ActingFor", core["ActingFor"], encode(t, Acting{ID: "user:a", Name: "A", RequestID: "r"}), true)
	match(t, core, "Group", core["Group"], encode(t, Group{ID: id, Name: "edge", Kind: "static", Revision: 1, MemberCount: &members, AgentIDs: []string{id}, Selector: "a"}), true)
	match(t, core, "GroupPage", core["GroupPage"], encode(t, GroupPage{Items: []Group{{ID: id, Name: "edge", Kind: "selector", Revision: 1}}, NextAfter: id}), false)
	match(t, core, "Agent", core["Agent"], encode(t, Agent{ID: id, Connected: true, CredentialID: "c", Attributes: map[string]string{"host.name": "h"}}), false)
	sha := strings.Repeat("f", 64)
	desired := DesiredConfig{Layers: []DesiredLayer{{Name: "10-base", AssignmentID: id, ConfigID: id, Version: 2, SHA256: sha}}, Hash: sha, Unmanaged: true}
	match(t, core, "DesiredConfig", core["DesiredConfig"], encode(t, desired), true)
	match(t, core, "AgentConfig", core["AgentConfig"], encode(t, AgentConfig{AgentID: id, Desired: &desired, Error: "too_many_layers"}), false)
	match(t, core, "Config", core["Config"], encode(t, StoredConfig{ID: id, Name: "base", Description: "d", Revision: 1, LatestVersion: 2, CreatedAt: now, CreatedBy: id}), true)
	version := ConfigVersion{ConfigID: id, Version: 2, SHA256: sha, Size: 1, Message: "m", CreatedAt: now, CreatedBy: id, Body: "receivers: {}"}
	match(t, core, "ConfigVersion", core["ConfigVersion"], encode(t, version), true)
	match(t, core, "ConfigVersionPage", core["ConfigVersionPage"], encode(t, ConfigVersionPage{Items: []ConfigVersion{version}, NextAfter: "2"}), true)
	match(t, core, "RolloutPage", core["RolloutPage"], encode(t, RolloutPage{Items: []Rollout{{ID: id, AssignmentID: id, ConfigID: id, FromVersion: 1, ToVersion: 2,
		State: "completed", Reason: "operator", CreatedAt: now, CreatedBy: id, UpdatedAt: now}}, NextAfter: id}), false)
}

func encode(t *testing.T, v any) any {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var decoded any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

// match checks that every encoded key is a schema property. With complete it
// also checks that every property and required key is encoded.
func match(t *testing.T, schemas map[string]any, path string, schema, value any, complete bool) {
	t.Helper()
	s, _ := schema.(map[string]any)
	if ref, ok := s["$ref"].(string); ok {
		s, _ = schemas[strings.TrimPrefix(ref, "#/components/schemas/")].(map[string]any)
	}
	if s == nil {
		t.Errorf("%s has no schema", path)
		return
	}
	switch v := value.(type) {
	case []any:
		for _, item := range v {
			match(t, schemas, path+"[]", s["items"], item, complete)
		}
	case map[string]any:
		properties, ok := s["properties"].(map[string]any)
		if !ok {
			return // a free-form map, such as attributes or drops by reason
		}
		for key, item := range v {
			if _, ok := properties[key]; !ok {
				t.Errorf("%s.%s is encoded but not in the contract", path, key)
				continue
			}
			match(t, schemas, path+"."+key, properties[key], item, complete)
		}
		if !complete {
			return
		}
		for key := range properties {
			if _, ok := v[key]; !ok {
				t.Errorf("%s.%s is in the contract but never encoded", path, key)
			}
		}
		required, _ := s["required"].([]any)
		for _, key := range required {
			if _, ok := v[key.(string)]; !ok {
				t.Errorf("%s.%s is required but not encoded", path, key)
			}
		}
	}
}
