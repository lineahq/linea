package artifact

import (
	"context"
	"fmt"
	"slices"
	"time"
)

// DefaultProject holds every record a request names no project for.
const DefaultProject = "default"

// LibraryProject holds the saved work shared with other projects.
const LibraryProject = "library"

// AllProjects in an audience or an availability list means every project.
const AllProjects = "*"

// Bounds of the project set.
const (
	MaxProjects       = 1000
	MaxAudiences      = 10000
	MaxAvailabilities = MaxRuntimeEntries + 1
	// MaxProjectList bounds the projects one audience or availability names.
	MaxProjectList = MaxProjects
	// MaxProjectName bounds a project's display name, in bytes.
	MaxProjectName = 128
)

// ValidProject reports whether id is a project ID: 1 to 64 lowercase letters,
// digits and hyphens, starting with a letter or digit.
func ValidProject(id string) bool {
	if id == "" || len(id) > 64 || id[0] == '-' {
		return false
	}
	for _, c := range []byte(id) {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// Projects holds the project registry and what is shared across projects:
// the audiences of library artifacts and the projects each runtime is
// available to. They form one set with one revision that every write raises;
// a write expecting another revision is ErrConflict. Every write records a
// ChangeProjects.
type Projects interface {
	// ProjectSet returns the set and its revision, 0 before the first write.
	ProjectSet(ctx context.Context) (ProjectSet, error)
	// SaveProject records p at revision expected, replacing the project of
	// the same ID. A new project beyond MaxProjects is ErrLimit.
	SaveProject(ctx context.Context, expected uint64, p ProjectRecord) (ProjectSet, error)
	// SetAudience records a's audience at revision expected; an empty
	// audience removes it. A new one beyond MaxAudiences is ErrLimit.
	SetAudience(ctx context.Context, expected uint64, a AudienceRecord) (ProjectSet, error)
	// SetAvailability records a runtime's availability at revision
	// expected; an empty list removes it. A new one beyond
	// MaxAvailabilities is ErrLimit.
	SetAvailability(ctx context.Context, expected uint64, a AvailabilityRecord) (ProjectSet, error)
}

// ProjectRecord is a project and its settings. A zero setting means the
// installation's. RetentionRuns and RetentionAge replace the installation's
// retention policy for the project's runs when either is set.
type ProjectRecord struct {
	ID             string
	Name           string
	Archived       bool
	DefaultRuntime string
	StorageLimit   int64
	RetentionRuns  int
	RetentionAge   time.Duration
	CreatedBy      string
	Created        time.Time
	UpdatedBy      string
	Updated        time.Time
}

// AudienceRecord is the projects a library artifact is shared with: project
// IDs, or AllProjects alone.
type AudienceRecord struct {
	ArtifactID string
	Projects   []string
	SetBy      string
	Set        time.Time
}

// AvailabilityRecord is the projects a runtime is available to: project
// IDs, or AllProjects alone. A runtime without one is available to every
// project.
type AvailabilityRecord struct {
	Runtime  string
	Projects []string
	SetBy    string
	Set      time.Time
}

// ProjectSet is the project registry and the sharing records at one
// revision, each ordered by ID.
type ProjectSet struct {
	Revision       uint64
	Projects       []ProjectRecord
	Audiences      []AudienceRecord
	Availabilities []AvailabilityRecord
}

// Project returns the project id, if recorded.
func (s ProjectSet) Project(id string) (ProjectRecord, bool) {
	i := slices.IndexFunc(s.Projects, func(p ProjectRecord) bool { return p.ID == id })
	if i < 0 {
		return ProjectRecord{}, false
	}
	return s.Projects[i], true
}

// Audience returns the audience of artifact id, if recorded.
func (s ProjectSet) Audience(id string) (AudienceRecord, bool) {
	i := slices.IndexFunc(s.Audiences, func(a AudienceRecord) bool { return a.ArtifactID == id })
	if i < 0 {
		return AudienceRecord{}, false
	}
	return s.Audiences[i], true
}

// Availability returns the availability of runtime name, if recorded.
func (s ProjectSet) Availability(name string) (AvailabilityRecord, bool) {
	i := slices.IndexFunc(s.Availabilities, func(a AvailabilityRecord) bool { return a.Runtime == name })
	if i < 0 {
		return AvailabilityRecord{}, false
	}
	return s.Availabilities[i], true
}

// Clone returns a copy of s that shares no slice with it.
func (s ProjectSet) Clone() ProjectSet {
	out := ProjectSet{Revision: s.Revision, Projects: slices.Clone(s.Projects),
		Audiences: make([]AudienceRecord, len(s.Audiences)), Availabilities: make([]AvailabilityRecord, len(s.Availabilities))}
	for i, a := range s.Audiences {
		a.Projects = slices.Clone(a.Projects)
		out.Audiences[i] = a
	}
	for i, a := range s.Availabilities {
		a.Projects = slices.Clone(a.Projects)
		out.Availabilities[i] = a
	}
	return out
}

// Includes reports whether a project list (IDs, or AllProjects alone)
// includes project.
func Includes(list []string, project string) bool {
	return slices.Contains(list, AllProjects) || slices.Contains(list, project)
}

// ValidateProject reports why a repository refuses p, as ErrInvalidRequest:
// a valid ID, a printable name of at most MaxProjectName bytes, a default
// runtime of at most 64 printable bytes, nonnegative settings, printable
// actors of at most 256 bytes and a creation time.
func ValidateProject(p ProjectRecord) error {
	switch {
	case !ValidProject(p.ID):
		return fmt.Errorf("%w: a project ID is 1 to 64 lowercase letters, digits and hyphens", ErrInvalidRequest)
	case len(p.Name) > MaxProjectName || !printable(p.Name):
		return fmt.Errorf("%w: a project name is at most %d printable bytes", ErrInvalidRequest, MaxProjectName)
	case len(p.DefaultRuntime) > 64 || !printable(p.DefaultRuntime):
		return fmt.Errorf("%w: a project's default runtime is at most 64 printable bytes", ErrInvalidRequest)
	case p.StorageLimit < 0 || p.RetentionRuns < 0 || p.RetentionAge < 0:
		return fmt.Errorf("%w: project settings are not negative", ErrInvalidRequest)
	case len(p.CreatedBy) > 256 || !printable(p.CreatedBy) || len(p.UpdatedBy) > 256 || !printable(p.UpdatedBy):
		return fmt.Errorf("%w: a project's actors are at most 256 printable bytes", ErrInvalidRequest)
	case p.Created.IsZero():
		return fmt.Errorf("%w: a project needs a creation time", ErrInvalidRequest)
	}
	return nil
}

// ValidateProjectList reports why a repository refuses an audience or an
// availability list: at most MaxProjectList distinct valid project IDs, or
// AllProjects alone. An empty list is valid and removes the record.
func ValidateProjectList(list []string) error {
	if len(list) > MaxProjectList {
		return fmt.Errorf("%w: at most %d projects", ErrInvalidRequest, MaxProjectList)
	}
	seen := map[string]bool{}
	for _, p := range list {
		switch {
		case p == AllProjects && len(list) != 1:
			return fmt.Errorf("%w: %q stands alone", ErrInvalidRequest, AllProjects)
		case p != AllProjects && !ValidProject(p):
			return fmt.Errorf("%w: %q is not a project ID", ErrInvalidRequest, p)
		case seen[p]:
			return fmt.Errorf("%w: project %s is named twice", ErrInvalidRequest, p)
		}
		seen[p] = true
	}
	return nil
}

// ValidateAudience reports why a repository refuses a.
func ValidateAudience(a AudienceRecord) error {
	if a.ArtifactID == "" || len(a.ArtifactID) > 128 || !printable(a.ArtifactID) {
		return fmt.Errorf("%w: an audience names its artifact", ErrInvalidRequest)
	}
	if len(a.SetBy) > 256 || !printable(a.SetBy) {
		return fmt.Errorf("%w: an audience's actor is at most 256 printable bytes", ErrInvalidRequest)
	}
	return ValidateProjectList(a.Projects)
}

// ValidateAvailability reports why a repository refuses a.
func ValidateAvailability(a AvailabilityRecord) error {
	if a.Runtime == "" || len(a.Runtime) > 64 || !printable(a.Runtime) {
		return fmt.Errorf("%w: an availability names its runtime", ErrInvalidRequest)
	}
	if len(a.SetBy) > 256 || !printable(a.SetBy) {
		return fmt.Errorf("%w: an availability's actor is at most 256 printable bytes", ErrInvalidRequest)
	}
	return ValidateProjectList(a.Projects)
}
