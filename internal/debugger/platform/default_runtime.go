package platform

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/authz"
)

// The default runtime is the runtime of every request that
// names none. The team chooses it in the repository; every instance holds a
// copy that follows the change feed. Until one is chosen, it is
// Options.DefaultRuntime, else the built-in runtime.

// DefaultRuntimeInfo is the default runtime: its name, and who chose it and
// when, both empty while none was chosen.
type DefaultRuntimeInfo struct {
	Name  string
	SetBy string
	Set   *time.Time
}

// defaultRuntimeName is the runtime a request that names none runs on, or
// empty for the built-in runtime in this process.
func (s *Service) defaultRuntimeName() string {
	s.defaultMu.Lock()
	chosen := s.chosenDefault.Name
	s.defaultMu.Unlock()
	if chosen != "" {
		return chosen
	}
	return s.unchosenDefault()
}

// unchosenDefault is the default runtime while the team has chosen none.
func (s *Service) unchosenDefault() string {
	switch {
	case s.defaultRuntime != "":
		return s.defaultRuntime
	case s.builtInOnWorkers:
		return BuiltInRuntime
	}
	return ""
}

// loadDefaultRuntime reads the chosen default from the repository again.
func (s *Service) loadDefaultRuntime(ctx context.Context) error {
	d, err := s.catalog.DefaultRuntime(ctx)
	if errors.Is(err, artifact.ErrNotFound) {
		d, err = artifact.DefaultRuntime{}, nil
	}
	if err != nil {
		return err
	}
	s.defaultMu.Lock()
	s.chosenDefault = d
	s.defaultMu.Unlock()
	return nil
}

// recoverDefaultRuntime reads the chosen default when the service opens.
func (s *Service) recoverDefaultRuntime() error {
	return s.loadDefaultRuntime(s.ctx)
}

func (s *Service) defaultRuntimeInfo() *DefaultRuntimeInfo {
	s.defaultMu.Lock()
	d := s.chosenDefault
	s.defaultMu.Unlock()
	info := &DefaultRuntimeInfo{Name: s.defaultRuntimeName(), SetBy: d.SetBy}
	if d.Name != "" {
		info.Set = &d.Set
	}
	if info.Name == "" {
		info.Name = BuiltInRuntime
	}
	return info
}

// GetDefaultRuntime returns the default runtime.
func (s *Service) GetDefaultRuntime(ctx context.Context, caller Caller) (_ *DefaultRuntimeInfo, err error) {
	defer s.audit(&caller, "GetDefaultRuntime", &err)()
	if err := s.begin(ctx, caller, authz.RuntimeRead); err != nil {
		return nil, err
	}
	if err := s.catchUp(ctx); err != nil {
		return nil, artifactError(err)
	}
	return s.defaultRuntimeInfo(), nil
}

// SetDefaultRuntime makes name, the built-in runtime or a runtime of the
// catalog, the default runtime of every instance. Its audit event names the
// previous default, then the new one.
func (s *Service) SetDefaultRuntime(ctx context.Context, caller Caller, name string) (_ *DefaultRuntimeInfo, err error) {
	defer s.audit(&caller, "SetDefaultRuntime", &err)()
	if err := s.begin(ctx, caller, authz.RuntimeManage); err != nil {
		return nil, err
	}
	entry := name != BuiltInRuntime
	if entry && !validRuntimeName(name) {
		return nil, &Error{Code: CodeInvalidRequest, Message: fmt.Sprintf("runtime name %q is invalid: name %s or a runtime of the catalog", name, BuiltInRuntime)}
	}
	d := artifact.DefaultRuntime{Name: name, SetBy: caller.Actor().Principal, Set: s.now().UTC()}
	previous, err := s.catalog.SetDefaultRuntime(ctx, d, entry)
	if errors.Is(err, artifact.ErrNotFound) {
		return nil, &Error{Code: CodeNotFound, Message: fmt.Sprintf("runtime %s is not in the catalog: register or build it first", name), cause: err}
	}
	if err != nil {
		return nil, classify(err, CodeStorageError)
	}
	if previous.Name == "" {
		previous.Name = s.unchosenDefault()
	}
	if previous.Name == "" {
		previous.Name = BuiltInRuntime
	}
	caller.touch(previous.Name)
	caller.touch(name)
	s.defaultMu.Lock()
	s.chosenDefault = d
	s.defaultMu.Unlock()
	return s.defaultRuntimeInfo(), nil
}
