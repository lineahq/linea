package livecapture

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/lineahq/linea/pluginsdk"
)

func TestPeerPageBudgetsEscapedCatalogsAndExpiresCursors(t *testing.T) {
	s, _, original := fixture(t)
	s.peers = map[string]*peer{}
	for n := 1; n <= 3; n++ {
		info := original.info
		info.ID = fmt.Sprintf("%064x", n)
		info.Identity.Points = nil
		for i := 0; i < 192; i++ {
			info.Identity.Points = append(info.Identity.Points, CatalogPoint{Pipeline: strings.Repeat("<", 128), Point: strings.Repeat(">", 120) + fmt.Sprintf("%08d", i), Signal: "traces", Sensitivity: "redacted"})
		}
		s.peers[info.ID] = &peer{info: info}
	}
	seen := map[string]bool{}
	query := "limit=64"
	var firstCursor string
	for {
		page, err := s.PeerPage(query)
		if err != nil || len(page.Items) != 1 {
			t.Fatal("byte budget did not split large catalogs", err)
		}
		encoded, _ := json.Marshal(page)
		if len(encoded) > PeerPageBytes {
			t.Fatal("encoded response exceeds ingress-safe budget")
		}
		if seen[page.Items[0].ID] {
			t.Fatal("duplicate page identity")
		}
		seen[page.Items[0].ID] = true
		if page.NextCursor == "" {
			break
		}
		if firstCursor == "" {
			firstCursor = page.NextCursor
		}
		query = "limit=64&cursor=" + url.QueryEscape(page.NextCursor)
	}
	if len(seen) != 3 {
		t.Fatal("pagination lost peers")
	}
	s.instance = pluginsdk.RandomID()
	if _, err := s.PeerPage("cursor=" + url.QueryEscape(firstCursor)); !errors.Is(err, ErrCursorExpired) {
		t.Fatal("old process cursor reused")
	}
	for _, query := range []string{"limit=0", "limit=65", "limit=1&limit=2", "cursor=", "cursor=bad", "unknown=1", "limit=%zz"} {
		if _, err := s.PeerPage(query); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid query accepted: %s", query)
		}
	}
}
