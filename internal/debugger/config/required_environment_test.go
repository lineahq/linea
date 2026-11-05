package config

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestRequiredEnvironmentNames(t *testing.T) {
	for _, schema := range []int{1, 2} {
		data := snapshotData{Schema: schema, URIs: []string{"yaml:root"}, Values: map[string]capturedValue{}}
		for uri, binding := range map[string]string{"env:Z_HOST": "host", "env:Z_HOST:-private-default": "host", "env:A_UNSET": "host_unset", "env:DECLARED": "declared", "env:DEFAULT:-private-default": "default", "file:private-path": ""} {
			if schema == 1 {
				binding = ""
			}
			data.Values[uri] = capturedValue{Binding: binding, Content: []byte("private-value")}
		}
		if schema == 2 {
			data.Environment = &EnvironmentPolicy{LegacyHost: true}
		}
		raw, _ := json.Marshal(data)
		s, err := DecodeSnapshot(raw, CaptureLimits{})
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"A_UNSET", "Z_HOST"}
		if schema == 1 {
			want = []string{"A_UNSET", "DECLARED", "DEFAULT", "Z_HOST"}
		}
		if got := s.RequiredEnvironment(); !reflect.DeepEqual(got, want) {
			t.Fatalf("schema %d: %v", schema, got)
		}
		got := s.RequiredEnvironment()
		got[0] = "mutated"
		if !reflect.DeepEqual(s.RequiredEnvironment(), want) {
			t.Fatal("returned slice aliases snapshot")
		}
	}
}
