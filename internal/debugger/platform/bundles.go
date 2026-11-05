package platform

import (
	"bytes"
	"context"
	"errors"

	"github.com/lineahq/linea/internal/debugger/authz"
)

var ErrBundleUnavailable = errors.New("run has no captured execution bundle")

// GetRunBundle returns private original input bytes for a retained run. The
// caller must treat them like source/secret data, not an ordinary metadata view.
// Legacy runs and explicit source-free test executors have no bundle.
func (s *Service) GetRunBundle(ctx context.Context, caller Caller, id string) (_ []byte, err error) {
	defer s.audit(&caller, "GetRunBundle", &err)()
	if err := s.begin(ctx, caller, authz.RunRead); err != nil {
		return nil, err
	}
	if err := s.runIn(ctx, caller, authz.RunRead, id); err != nil {
		return nil, err
	}
	return s.getRunBundle(ctx, id)
}

func (s *Service) getRunBundle(ctx context.Context, id string) ([]byte, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, classify(ErrClosed, CodeUnavailable)
	}
	r, ok := s.runs[id]
	repository := s.repository
	unknown := s.unknownRunLocked(id)
	s.mu.Unlock()
	if !ok {
		return nil, classify(unknown, CodeNotFound)
	}
	r.mu.Lock()
	info := r.bundleInfo
	raw := r.bundle
	r.mu.Unlock()
	if info == nil {
		return nil, classify(ErrBundleUnavailable, CodeNotFound)
	}
	if repository != nil {
		raw, err := repository.RunBundle(ctx, id)
		return raw, classify(err, CodeStorageError)
	}
	return bytes.Clone(raw), nil
}
