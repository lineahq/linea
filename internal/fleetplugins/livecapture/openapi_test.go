package livecapture

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/pluginsdk"
)

// The published schemas name exactly the fields the plugin's types encode.
// Values set every optional field, so a key on either side alone is drift.
func TestOpenAPIMatchesWireTypes(t *testing.T) {
	var document struct {
		Components struct {
			Schemas map[string]any `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(openAPI, &document); err != nil {
		t.Fatal(err)
	}
	schemas := document.Components.Schemas
	id, peer, now := strings.Repeat("a", 32), strings.Repeat("b", 64), time.Now().UTC()
	stats := &TargetStats{Selected: 1, EnqueuedRecords: 1, EnqueuedBytes: 1, FilterErrors: 1, Dropped: map[string]int64{"sampling": 1}}
	points := []CatalogPoint{{Pipeline: "traces", Point: "egress", Signal: "traces", Sensitivity: "redacted"}}
	filter := Filter{Resource: []string{"true"}}
	filter.Traces.Span = []string{"true"}
	filter.Logs.Log = []string{"true"}
	filter.Metrics.Metric = []string{"true"}
	filter.Metrics.Datapoint = []string{"true"}
	for name, value := range map[string]any{
		"Envelope":  Envelope{AuthorityID: id, PluginInstance: id, PluginEpoch: id, ID: id, CaptureID: id, PeerID: peer, Generation: 1, Signal: "traces", Records: 1, Encoding: "otlp-protobuf", Data: []byte{1}, ObservedAt: now},
		"Operation": Operation{AuthorityID: id, ID: id, State: "active", Reason: "r", Revision: 1, ExpiresAt: now, Targets: []Target{{PeerID: peer, Generation: 1, AgentID: id, State: "active", Stats: stats}}, ActingFor: &pluginsdk.Acting{ID: "user:alice", Name: "Alice", RequestID: "r"}},
		"Peer": PeerInfo{AuthorityID: id, PluginInstance: id, ID: peer, Generation: 1, CredentialID: "c", AgentID: id, Identity: Identity{Cluster: "c", Namespace: "n", Pod: "p", Group: "g",
			CollectorVersion: "v", DistributionVersion: "v", Enabled: true, Points: points, FilterLimits: v1FilterLimits}},
		"CaptureRequest": CreateRequest{AuthorityID: id, PluginInstance: id, ID: id, ChannelID: id, Targets: []TargetRequest{{PeerID: peer, Generation: 1}}, Agents: []string{id}, Selector: "a", GroupID: id,
			Points: []Point{{Pipeline: "traces", Point: "egress"}}, Signals: []string{"traces"}, Filter: filter, Sampling: &Sampling{Rate: 1, Mode: "hash"},
			Limits: Limits{BytesPerSecond: 1, RecordsPerSecond: 1, TotalBytes: 1, TotalRecords: 1, RecordBytes: 1}, ExpiresAt: now},
		"Ack": Acknowledgement{ID: id, Accepted: 1},
	} {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var decoded any
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatal(err)
		}
		matchSchema(t, schemas, name, schemas[name], decoded)
	}
}

func matchSchema(t *testing.T, schemas map[string]any, path string, schema, value any) {
	t.Helper()
	s, _ := schema.(map[string]any)
	if ref, ok := s["$ref"].(string); ok {
		s, _ = schemas[strings.TrimPrefix(ref, "#/components/schemas/")].(map[string]any)
	}
	switch v := value.(type) {
	case []any:
		for _, item := range v {
			matchSchema(t, schemas, path+"[]", s["items"], item)
		}
	case map[string]any:
		properties, ok := s["properties"].(map[string]any)
		if !ok {
			return // a free-form map, such as dropped counts by reason
		}
		for key, item := range v {
			if _, ok := properties[key]; !ok {
				t.Errorf("%s.%s is encoded but not in the OpenAPI", path, key)
				continue
			}
			matchSchema(t, schemas, path+"."+key, properties[key], item)
		}
		for key := range properties {
			if _, ok := v[key]; !ok {
				t.Errorf("%s.%s is in the OpenAPI but never encoded", path, key)
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
