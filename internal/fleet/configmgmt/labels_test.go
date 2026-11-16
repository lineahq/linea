package configmgmt

import (
	"errors"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/fleet/fleet"
)

func TestAssignmentsRequireCredentialLabels(t *testing.T) {
	a := Assignment{ID: strings.Repeat("e1", 16), Name: "base", ConfigID: strings.Repeat("c1", 16), Version: 1, Layer: 10, Selector: "a=b"}
	valid := func(map[string]string) bool { return true }
	if _, err := NormalizeAssignment(a, valid); !errors.Is(err, fleet.ErrInvalid) {
		t.Fatalf("assignment without credential labels: %v", err)
	}
	a.CredentialLabels = map[string]string{"env": "prod"}
	if _, err := NormalizeAssignment(a, valid); err != nil {
		t.Fatal(err)
	}
	// A target stored without labels matches no agent.
	tg, err := NewTarget(Assignment{ID: "x", Name: "x", ConfigID: "c", Version: 1, Layer: 10}, []byte{1}, "", []string{"agent"})
	if err != nil {
		t.Fatal(err)
	}
	if tg.Matches("agent", nil, map[string]string{"env": "prod"}) {
		t.Fatal("a label-less target matched")
	}
}
