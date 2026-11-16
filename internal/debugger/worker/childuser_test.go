package worker

import "testing"

func TestParseChildUser(t *testing.T) {
	tests := []struct {
		in   string
		want *ChildUser
	}{
		{"65534:65534", &ChildUser{UID: 65534, GID: 65534}},
		{"1000:2000", &ChildUser{UID: 1000, GID: 2000}},
		{"65534", nil},
		{"0:65534", nil},
		{"65534:0", nil},
		{"nobody:nogroup", nil},
		{"-1:5", nil},
		{"4294967296:5", nil},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseChildUser(tt.in)
			switch {
			case tt.want == nil && err == nil:
				t.Fatalf("ParseChildUser(%q) = %v, want an error", tt.in, got)
			case tt.want != nil && (err != nil || *got != *tt.want):
				t.Fatalf("ParseChildUser(%q) = %v, %v; want %v", tt.in, got, err, tt.want)
			}
		})
	}
}
