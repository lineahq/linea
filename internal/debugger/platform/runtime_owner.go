package platform

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/authz"
)

// mayTakeRuntimeName checks that caller may build into name on a team
// server. A free name needs only runtime.build. A registered name may be
// replaced by the principal that registered it, or with runtime.manage;
// such a replacement is audited as its own ReplaceRuntime event.
func (s *Service) mayTakeRuntimeName(ctx context.Context, caller Caller, name string) error {
	e, err := s.catalog.Runtime(ctx, name)
	switch {
	case errors.Is(err, artifact.ErrNotFound):
		return nil
	case err != nil:
		return artifactError(err)
	case e.RegisteredBy == caller.Principal.ID:
		return nil
	}
	allowed := s.effective.Load().Allowed(identity(caller.Principal), authz.RuntimeManage, authz.All)
	if !allowed {
		caller.decide(authz.RuntimeManage, "runtime:"+name, false)
		return &Error{Code: CodeForbidden, Message: fmt.Sprintf(
			"forbidden: runtime %s was registered by another principal; replacing it needs the %s permission", name, authz.RuntimeManage)}
	}
	replaced := caller
	replaced.audit = nil
	s.record(replaced, "ReplaceRuntime", &auditRecord{permission: authz.RuntimeManage, scope: "runtime:" + name}, nil)
	return nil
}

// takenSince reports whether another principal than principal registered
// name after since, which a build requested at since must not replace.
func (s *Service) takenSince(ctx context.Context, name, principal string, since time.Time) (bool, error) {
	e, err := s.catalog.Runtime(ctx, name)
	switch {
	case errors.Is(err, artifact.ErrNotFound):
		return false, nil
	case err != nil:
		return false, err
	}
	return e.RegisteredBy != principal && e.Registered.After(since), nil
}
