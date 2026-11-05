package artifact

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"slices"
)

// A run cursor continues a run list below an admission ordinal. It carries
// a digest of the query it belongs to, so it never continues another one.
// Ordinals only grow and deleted runs leave gaps, so a cursor stays valid
// while runs are admitted or removed.
const runCursorBytes = 8 + 16

// digest identifies q's projects and filters, not its page.
func (q RunQuery) digest() []byte {
	projects := slices.Clone(q.Projects)
	slices.Sort(projects)
	raw, _ := json.Marshal(struct {
		All        bool     `json:"all"`
		Projects   []string `json:"projects"`
		Scenario   string   `json:"scenario"`
		Runtime    string   `json:"runtime"`
		Result     string   `json:"result"`
		Unfinished bool     `json:"unfinished"`
		Unindexed  bool     `json:"unindexed"`
	}{q.Projects == nil, projects, q.Scenario, q.Runtime, q.Result, q.Unfinished, q.Unindexed})
	sum := sha256.Sum256(raw)
	return sum[:16]
}

// Normalize checks q and returns it with its limit set, and the ordinal the
// page starts below: zero for the first page.
func (q RunQuery) Normalize() (RunQuery, uint64, error) {
	if !(RunIndex{Scenario: q.Scenario, Runtime: q.Runtime, Result: q.Result}).Valid() || q.Limit < 0 || q.Limit > MaxRunPage {
		return q, 0, fmt.Errorf("%w: invalid run query", ErrInvalidRequest)
	}
	if q.Limit == 0 {
		q.Limit = DefaultRunPage
	}
	if q.Cursor == "" {
		return q, 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(q.Cursor)
	if err != nil || len(raw) != runCursorBytes || binary.BigEndian.Uint64(raw[:8]) == 0 {
		return q, 0, fmt.Errorf("%w: invalid run cursor", ErrInvalidRequest)
	}
	before := binary.BigEndian.Uint64(raw[:8])
	if string(raw[8:]) != string(q.digest()) {
		return q, 0, fmt.Errorf("%w: the run cursor belongs to another query", ErrInvalidRequest)
	}
	return q, before, nil
}

// NextCursor is the cursor of the page after one that ended with the run of
// ordinal last.
func (q RunQuery) NextCursor(last uint64) string {
	raw := binary.BigEndian.AppendUint64(make([]byte, 0, runCursorBytes), last)
	return base64.RawURLEncoding.EncodeToString(append(raw, q.digest()...))
}

// Matches reports whether run r belongs to q's projects and filters; a run
// without an index matches no index filter.
func (q RunQuery) Matches(r RunRecord) bool {
	x := r.Index
	if q.Projects != nil && !slices.Contains(q.Projects, r.Project) || q.Unfinished && r.Terminal || q.Unindexed && x != nil {
		return false
	}
	if q.Scenario == "" && q.Runtime == "" && q.Result == "" {
		return true
	}
	return x != nil && (q.Scenario == "" || x.Scenario == q.Scenario) && (q.Runtime == "" || x.Runtime == q.Runtime) && (q.Result == "" || x.Result == q.Result)
}
