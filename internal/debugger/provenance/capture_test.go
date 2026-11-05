package provenance

import (
	"strings"
	"testing"
)

func TestValidateCaptureOrigins(t *testing.T) {
	valid := func() CaptureDerivation {
		return CaptureDerivation{SessionID: "capture-test", Selection: CaptureInputSelection{Stream: CaptureStream{CollectorID: "collector-test", Pipeline: "logs", Point: "egress", Signal: "logs"}, Watermark: 1, Sequences: []int{1}, Receiver: "otlp"}, SourceDigest: strings.Repeat("a", 64), Digest: strings.Repeat("b", 64), DestinationConfigDigest: strings.Repeat("c", 64), Records: 1}
	}
	if err := ValidateCaptures(nil); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCaptures([]CaptureDerivation{valid()}); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"", "mtls", "tls", "plaintext"} {
		v := valid()
		v.Source = &CaptureSource{Pod: "pod-0", CollectorGroup: "lab", CollectorVersion: "0.162.0", TransportSecurity: mode}
		if err := ValidateCaptures([]CaptureDerivation{v}); err != nil {
			t.Fatalf("valid source transport %q: %v", mode, err)
		}
	}
	for _, test := range []struct {
		name   string
		mutate func(*CaptureDerivation)
	}{
		{"oversized_digest", func(v *CaptureDerivation) { v.SourceDigest = strings.Repeat("a", 1<<20) }},
		{"uppercase_digest", func(v *CaptureDerivation) { v.Digest = strings.Repeat("B", 64) }},
		{"duplicate_sequence", func(v *CaptureDerivation) { v.Selection.Sequences = []int{1, 1}; v.Records = 2 }},
		{"watermark", func(v *CaptureDerivation) { v.Selection.Watermark = 0 }},
		{"signal", func(v *CaptureDerivation) { v.Selection.Stream.Signal = "profiles" }},
		{"control_character", func(v *CaptureDerivation) { v.Selection.Stream.CollectorID = "collector\n" }},
		{"source_authentication", func(v *CaptureDerivation) {
			v.Source = &CaptureSource{Pod: "pod-0", CollectorGroup: "lab", CollectorVersion: "0.162.0", Authentication: "assumed_authenticated"}
		}},
		{"source_transport", func(v *CaptureDerivation) {
			v.Source = &CaptureSource{Pod: "pod-0", CollectorGroup: "lab", CollectorVersion: "0.162.0", TransportSecurity: "assumed_secure"}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			v := valid()
			test.mutate(&v)
			if err := ValidateCaptures([]CaptureDerivation{v}); err == nil {
				t.Fatal("invalid origin accepted")
			}
		})
	}
	v := valid()
	v.Selection.Watermark = 1000
	v.Selection.Sequences = make([]int, 1000)
	for i := range v.Selection.Sequences {
		v.Selection.Sequences[i] = i + 1
	}
	v.Records = 1000
	origins := make([]CaptureDerivation, 64)
	for i := range origins {
		origins[i] = v
	}
	if err := ValidateCaptures(origins); err == nil {
		t.Fatal("oversized provenance document accepted")
	}
}
