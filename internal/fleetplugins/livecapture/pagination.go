package livecapture

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"sort"
	"strconv"

	"github.com/lineahq/linea/pluginsdk"
)

const PeerPageBytes = 512 << 10

type PeerPage struct {
	Items      []PeerInfo `json:"items"`
	NextCursor string     `json:"next_cursor,omitempty"`
}
type peerCursor struct {
	Authority string `json:"authority"`
	Instance  string `json:"instance"`
	After     string `json:"after"`
}

func (s *Server) PeerPage(rawQuery string) (PeerPage, error) {
	page := PeerPage{Items: []PeerInfo{}}
	query, err := url.ParseQuery(rawQuery)
	if err != nil || len(rawQuery) > 2048 {
		return page, ErrInvalid
	}
	for key, values := range query {
		if (key != "limit" && key != "cursor") || len(values) != 1 || values[0] == "" {
			return page, ErrInvalid
		}
	}
	limit := 16
	if value := query.Get("limit"); value != "" {
		limit, err = strconv.Atoi(value)
		if err != nil || limit < 1 || limit > 64 {
			return page, ErrInvalid
		}
	}
	var cursor peerCursor
	if value := query.Get("cursor"); value != "" {
		if len(value) > 512 {
			return page, ErrInvalid
		}
		data, err := base64.RawURLEncoding.DecodeString(value)
		if err != nil || strictJSON(data, &cursor) != nil || !validPeerID(cursor.After) || !pluginsdk.ValidUID(cursor.Authority) || !pluginsdk.ValidUID(cursor.Instance) {
			return page, ErrInvalid
		}
		canonical, _ := json.Marshal(cursor)
		if base64.RawURLEncoding.EncodeToString(canonical) != value {
			return page, ErrInvalid
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if cursor.After != "" && (cursor.Authority != s.repo.AuthorityID() || cursor.Instance != s.instance) {
		return page, ErrCursorExpired
	}
	ids := make([]string, 0, len(s.peers))
	for id := range s.peers {
		if id > cursor.After {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	// Reserve space for the envelope and the bounded next cursor. Budget the
	// actual JSON encoding (including escaping), not reported string lengths.
	bytes := 1024
	for _, id := range ids {
		info := s.peers[id].info
		encoded, err := json.Marshal(info)
		if err != nil {
			return page, ErrUnavailable
		}
		if len(page.Items) >= limit || bytes+len(encoded)+1 > PeerPageBytes {
			if len(page.Items) == 0 {
				return page, ErrCapacity
			}
			last := page.Items[len(page.Items)-1].ID
			data, _ := json.Marshal(peerCursor{Authority: s.repo.AuthorityID(), Instance: s.instance, After: last})
			page.NextCursor = base64.RawURLEncoding.EncodeToString(data)
			break
		}
		info.Identity.Points = append([]CatalogPoint(nil), info.Identity.Points...)
		page.Items = append(page.Items, info)
		bytes += len(encoded) + 1
	}
	return page, nil
}
