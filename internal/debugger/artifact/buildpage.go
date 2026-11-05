package artifact

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
)

// A build cursor continues a build list below an ordinal, as a run cursor
// does, and carries a digest of the query's principal so that it never
// continues another query.
const buildCursorBytes = 8 + 16

func (q BuildQuery) digest() []byte {
	sum := sha256.Sum256([]byte("linea.build.query\x00" + q.Principal))
	return sum[:16]
}

// Normalize checks q and returns it with its limit set (zero is
// DefaultBuildPage, and a limit beyond MaxBuildPage is MaxBuildPage), and
// the ordinal the page starts below: zero for the first page.
func (q BuildQuery) Normalize() (BuildQuery, uint64, error) {
	if q.Limit < 0 {
		return q, 0, fmt.Errorf("%w: invalid build query", ErrInvalidRequest)
	}
	switch {
	case q.Limit == 0:
		q.Limit = DefaultBuildPage
	case q.Limit > MaxBuildPage:
		q.Limit = MaxBuildPage
	}
	if q.Cursor == "" {
		return q, 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(q.Cursor)
	if err != nil || len(raw) != buildCursorBytes || binary.BigEndian.Uint64(raw[:8]) == 0 {
		return q, 0, fmt.Errorf("%w: invalid build cursor", ErrInvalidRequest)
	}
	if string(raw[8:]) != string(q.digest()) {
		return q, 0, fmt.Errorf("%w: the build cursor belongs to another query", ErrInvalidRequest)
	}
	return q, binary.BigEndian.Uint64(raw[:8]), nil
}

// NextCursor is the cursor of the page after one that ended with the build
// of ordinal last.
func (q BuildQuery) NextCursor(last uint64) string {
	raw := binary.BigEndian.AppendUint64(make([]byte, 0, buildCursorBytes), last)
	return base64.RawURLEncoding.EncodeToString(append(raw, q.digest()...))
}
