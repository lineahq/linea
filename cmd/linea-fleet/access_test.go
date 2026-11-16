package main

import (
	"maps"
	"testing"
)

func TestParsePluginAccess(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  map[string]string
		ok    bool
	}{
		{"", nil, true},
		{"livecapture=write", map[string]string{"livecapture": "write"}, true},
		{"livecapture=read, agent-summary=admin", map[string]string{"livecapture": "read", "agent-summary": "admin"}, true},
		{"livecapture", nil, false},
		{"livecapture=owner", nil, false},
		{"livecapture=read,livecapture=write", nil, false},
		{"Live=read", nil, false},
		{",", nil, false},
	} {
		got, err := parsePluginAccess(tc.value)
		if (err == nil) != tc.ok || !maps.Equal(got, tc.want) {
			t.Errorf("%q: got %v, %v", tc.value, got, err)
		}
	}
}
