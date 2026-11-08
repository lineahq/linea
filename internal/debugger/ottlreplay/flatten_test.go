package ottlreplay

import "testing"

// The shapes of the transform processor's Config across the support table.
type (
	configUpTo0163 struct{ FlattenData bool }
	flattenConfig  struct{ Log bool }
	config0164     struct {
		Flatten     flattenConfig
		FlattenData bool
	}
	configFrom0165 struct{ Flatten flattenConfig }
)

func TestFlattensLogsReadsEveryReleaseShape(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  any
		want bool
	}{
		{"0.163 flatten_data", &configUpTo0163{FlattenData: true}, true},
		{"0.163 unset", &configUpTo0163{}, false},
		{"0.164 flatten_data", &config0164{FlattenData: true}, true},
		{"0.164 flatten.log", &config0164{Flatten: flattenConfig{Log: true}}, true},
		{"0.164 unset", &config0164{}, false},
		{"0.165 flatten.log", &configFrom0165{Flatten: flattenConfig{Log: true}}, true},
		{"0.165 unset", &configFrom0165{}, false},
		{"by value", configFrom0165{Flatten: flattenConfig{Log: true}}, true},
		{"nil pointer", (*configFrom0165)(nil), false},
		{"nil", nil, false},
		{"not a struct", "flatten", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := flattensLogs(tc.cfg); got != tc.want {
				t.Fatalf("flattensLogs = %v, want %v", got, tc.want)
			}
		})
	}
}
