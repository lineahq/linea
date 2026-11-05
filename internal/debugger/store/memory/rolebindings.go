package memory

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/lineahq/linea/internal/debugger/artifact"
)

// RoleBindings returns the managed role bindings and their revision.
func (s *Store) RoleBindings(ctx context.Context) (artifact.RoleBindingSet, error) {
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return artifact.RoleBindingSet{}, err
	}
	return s.d.roleBindingSet(), nil
}

// AddRoleBinding adds a managed role binding at revision expected.
func (s *Store) AddRoleBinding(ctx context.Context, expected uint64, b artifact.RoleBindingRecord) (artifact.RoleBindingSet, error) {
	if err := artifact.ValidateRoleBinding(b); err != nil {
		return artifact.RoleBindingSet{}, err
	}
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return artifact.RoleBindingSet{}, err
	}
	set := &s.d.roleBindings
	if set.Revision != expected {
		return artifact.RoleBindingSet{}, fmt.Errorf("%w: role bindings are at revision %d, not %d", artifact.ErrConflict, set.Revision, expected)
	}
	for _, existing := range set.Bindings {
		if existing.ID == b.ID || existing.SameGrant(b) {
			return artifact.RoleBindingSet{}, fmt.Errorf("%w: the role binding exists", artifact.ErrConflict)
		}
	}
	if len(set.Bindings) >= artifact.MaxRoleBindings {
		return artifact.RoleBindingSet{}, artifact.ErrLimit
	}
	b.Created = b.Created.UTC()
	set.Bindings = append(slices.Clone(set.Bindings), b)
	slices.SortStableFunc(set.Bindings, func(x, y artifact.RoleBindingRecord) int {
		if c := x.Created.Compare(y.Created); c != 0 {
			return c
		}
		return strings.Compare(x.ID, y.ID)
	})
	set.Revision++
	s.d.change(artifact.ChangeRoleBindings, "bindings")
	return s.d.roleBindingSet(), nil
}

// RemoveRoleBinding removes a managed role binding at revision expected.
func (s *Store) RemoveRoleBinding(ctx context.Context, expected uint64, id string) (artifact.RoleBindingSet, error) {
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return artifact.RoleBindingSet{}, err
	}
	set := &s.d.roleBindings
	if set.Revision != expected {
		return artifact.RoleBindingSet{}, fmt.Errorf("%w: role bindings are at revision %d, not %d", artifact.ErrConflict, set.Revision, expected)
	}
	i := slices.IndexFunc(set.Bindings, func(b artifact.RoleBindingRecord) bool { return b.ID == id })
	if i < 0 {
		return artifact.RoleBindingSet{}, fmt.Errorf("%w: role binding %s", artifact.ErrNotFound, id)
	}
	set.Bindings = slices.Delete(slices.Clone(set.Bindings), i, i+1)
	set.Revision++
	s.d.change(artifact.ChangeRoleBindings, "bindings")
	return s.d.roleBindingSet(), nil
}

// roleBindingSet is a copy of the managed role bindings; the caller holds
// d.mu.
func (d *Data) roleBindingSet() artifact.RoleBindingSet {
	return artifact.RoleBindingSet{Revision: d.roleBindings.Revision, Bindings: slices.Clone(d.roleBindings.Bindings)}
}
