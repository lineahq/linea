package platform

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/worker"
)

// Suspect binaries. A pool worker that finds a runtime binary
// wrong (its digest, its start, the runtime it announces) marks its answer
// worker.ErrSuspectBinary. The service records the first such answer on
// every catalog entry of that binary, refuses runs on them at admission and
// fails runs already pinned to it before they reach a pool. Registering or
// building the runtime again saves its entry anew, which clears the mark.

// urlPattern finds URLs, presigned ones included, in a recorded cause.
var urlPattern = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://\S+`)

// suspicionCause is err's message as a suspicion records it: URLs removed,
// control characters replaced and at most artifact.MaxSuspicionCause bytes.
func suspicionCause(err error) string {
	cause := urlPattern.ReplaceAllString(err.Error(), "<url>")
	cause = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, cause)
	if len(cause) > artifact.MaxSuspicionCause {
		cut := artifact.MaxSuspicionCause
		for cut > 0 && !utf8.RuneStart(cause[cut]) {
			cut--
		}
		cause = cause[:cut]
	}
	if cause == "" {
		cause = worker.ErrSuspectBinary.Error()
	}
	return cause
}

// suspected records binary as suspect when pool e answered err for it, and
// returns err.
func (s *Service) suspected(e *executor, b *worker.PoolBinary, err error) error {
	if b == nil || e.pool() == "" || !errors.Is(err, worker.ErrSuspectBinary) {
		return err
	}
	m := artifact.Suspicion{Cause: suspicionCause(err), Pool: e.presence(), At: s.now().UTC()}
	if _, merr := s.catalog.MarkSuspect(s.ctx, b.Digest, m); merr != nil {
		s.logError("recording a suspect runtime binary", merr)
	} else if s.logger != nil {
		s.logger.Warn("scheduler: a pool distrusts a runtime binary; runs on it fail until it is registered or built again")
	}
	return err
}

// suspectBinary fails a request for runtime name on the stored binary
// digest when the catalog records it as suspect.
func (s *Service) suspectBinary(ctx context.Context, name, digest string) error {
	entries, err := s.catalog.RuntimeEntries(ctx)
	if err != nil {
		return classify(err, CodeStorageError)
	}
	for _, e := range entries {
		if e.Suspect != nil && e.Binary != nil && e.Binary.Digest == digest {
			return suspectError(name, e.Suspect)
		}
	}
	return nil
}

// suspectError says why runtime name is unavailable.
func suspectError(name string, m *artifact.Suspicion) *Error {
	return &Error{Code: CodeUnavailable, Message: fmt.Sprintf(
		"runtime %s is not available: %s found its binary wrong at %s (%s); register or build it again",
		name, m.Pool, m.At.Format("2006-01-02T15:04:05Z"), m.Cause)}
}
