// Package auth implements the proof server's independent bootstrap credentials.
package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
)

type Bearer struct{ digest [32]byte }

func New(secret string) (Bearer, error) {
	if len(secret) < 32 || len(secret) > 1024 || strings.ContainsAny(secret, " \t\r\n") {
		return Bearer{}, errors.New("bearer secret must contain 32 to 1024 bytes without whitespace")
	}
	return Bearer{digest: sha256.Sum256([]byte(secret))}, nil
}

func (b Bearer) Valid(r *http.Request) bool {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return false
	}
	scheme, token, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || len(token) > 1024 {
		return false
	}
	digest := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(digest[:], b.digest[:]) == 1
}
