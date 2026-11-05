package platform

import (
	"strings"
	"testing"
)

// TestRegisteringPicksTheDigestOfTheImagesRepository reads the digest a
// registration records from the image's repository references.
func TestRegisteringPicksTheDigestOfTheImagesRepository(t *testing.T) {
	a, b := "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64)
	digests := []string{"mirror.example/linea@" + a, "registry.example:5000/team/linea@" + b}
	for _, tc := range []struct {
		name, ref string
		digests   []string
		want      string
	}{
		{"tagged", "registry.example:5000/team/linea:edge", digests, b},
		{"untagged", "registry.example:5000/team/linea", digests, b},
		{"by digest", "registry.example:5000/team/linea@" + b, digests, b},
		{"other repository", "local/linea:edge", digests, a},
		{"never pushed", "linea-runtime:edge", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := repoDigest(tc.digests, tc.ref); got != tc.want {
				t.Fatalf("repoDigest(%s) = %q, want %q", tc.ref, got, tc.want)
			}
		})
	}
}
