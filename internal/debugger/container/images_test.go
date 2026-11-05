package container

import (
	"slices"
	"testing"
)

func TestAnImageWithSeveralTagsIsListedOnce(t *testing.T) {
	out := "sha256:aa\nsha256:bb\nsha256:aa\n\nsha256:cc\nsha256:bb\n"
	if got, want := imageIDs(out), []string{"sha256:aa", "sha256:bb", "sha256:cc"}; !slices.Equal(got, want) {
		t.Fatalf("imageIDs = %q, want %q", got, want)
	}
	if got := imageIDs(""); len(got) != 0 {
		t.Fatalf("imageIDs of nothing = %q", got)
	}
}
