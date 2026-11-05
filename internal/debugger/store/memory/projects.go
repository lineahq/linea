package memory

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/lineahq/linea/internal/debugger/artifact"
)

// ProjectSet returns the project set and its revision.
func (s *Store) ProjectSet(ctx context.Context) (artifact.ProjectSet, error) {
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return artifact.ProjectSet{}, err
	}
	return s.d.projects.Clone(), nil
}

// SaveProject records a project at revision expected.
func (s *Store) SaveProject(ctx context.Context, expected uint64, p artifact.ProjectRecord) (artifact.ProjectSet, error) {
	if err := artifact.ValidateProject(p); err != nil {
		return artifact.ProjectSet{}, err
	}
	p.Created, p.Updated = p.Created.UTC(), p.Updated.UTC()
	return s.writeProjects(ctx, expected, func(set *artifact.ProjectSet) error {
		i := slices.IndexFunc(set.Projects, func(x artifact.ProjectRecord) bool { return x.ID == p.ID })
		if i >= 0 {
			set.Projects[i] = p
			return nil
		}
		if len(set.Projects) >= artifact.MaxProjects {
			return artifact.ErrLimit
		}
		set.Projects = append(set.Projects, p)
		slices.SortFunc(set.Projects, func(a, b artifact.ProjectRecord) int { return strings.Compare(a.ID, b.ID) })
		return nil
	})
}

// SetAudience records a library artifact's audience at revision expected.
func (s *Store) SetAudience(ctx context.Context, expected uint64, a artifact.AudienceRecord) (artifact.ProjectSet, error) {
	if err := artifact.ValidateAudience(a); err != nil {
		return artifact.ProjectSet{}, err
	}
	a.Projects, a.Set = slices.Clone(a.Projects), a.Set.UTC()
	return s.writeProjects(ctx, expected, func(set *artifact.ProjectSet) error {
		var err error
		set.Audiences, err = upsert(set.Audiences, a, len(a.Projects) == 0, artifact.MaxAudiences,
			func(x artifact.AudienceRecord) string { return x.ArtifactID })
		return err
	})
}

// SetAvailability records a runtime's availability at revision expected.
func (s *Store) SetAvailability(ctx context.Context, expected uint64, a artifact.AvailabilityRecord) (artifact.ProjectSet, error) {
	if err := artifact.ValidateAvailability(a); err != nil {
		return artifact.ProjectSet{}, err
	}
	a.Projects, a.Set = slices.Clone(a.Projects), a.Set.UTC()
	return s.writeProjects(ctx, expected, func(set *artifact.ProjectSet) error {
		var err error
		set.Availabilities, err = upsert(set.Availabilities, a, len(a.Projects) == 0, artifact.MaxAvailabilities,
			func(x artifact.AvailabilityRecord) string { return x.Runtime })
		return err
	})
}

// writeProjects applies write to a copy of the set at revision expected
// and keeps it, with a raised revision, when write succeeds.
func (s *Store) writeProjects(ctx context.Context, expected uint64, write func(*artifact.ProjectSet) error) (artifact.ProjectSet, error) {
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return artifact.ProjectSet{}, err
	}
	if s.d.projects.Revision != expected {
		return artifact.ProjectSet{}, fmt.Errorf("%w: projects are at revision %d, not %d", artifact.ErrConflict, s.d.projects.Revision, expected)
	}
	next := s.d.projects.Clone()
	if err := write(&next); err != nil {
		return artifact.ProjectSet{}, err
	}
	next.Revision++
	s.d.projects = next
	s.d.change(artifact.ChangeProjects, "projects")
	return next.Clone(), nil
}

// upsert replaces the record of r's key in list, removes it when remove is
// set, or adds r in key order within max.
func upsert[T any](list []T, r T, remove bool, max int, key func(T) string) ([]T, error) {
	i := slices.IndexFunc(list, func(x T) bool { return key(x) == key(r) })
	switch {
	case remove && i >= 0:
		return slices.Delete(list, i, i+1), nil
	case remove:
		return list, nil
	case i >= 0:
		list[i] = r
		return list, nil
	case len(list) >= max:
		return nil, artifact.ErrLimit
	}
	list = append(list, r)
	slices.SortFunc(list, func(a, b T) int { return strings.Compare(key(a), key(b)) })
	return list, nil
}
