package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// Projects, the library and sharing. Every request may name the project it
// works in with ?project=<id>; the platform decides what that allows.

// Project is a project the caller may see, with the project-scoped
// permissions the caller holds in it.
type Project struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	BuiltIn  bool   `json:"built_in"`
	Archived bool   `json:"archived"`
	ProjectSettings
	Permissions []string   `json:"permissions"`
	CreatedBy   string     `json:"created_by,omitempty"`
	Created     *time.Time `json:"created,omitempty"`
	UpdatedBy   string     `json:"updated_by,omitempty"`
	Updated     *time.Time `json:"updated,omitempty"`
}

// ProjectSettings are a project's own settings; zero values fall back to
// the server's.
type ProjectSettings struct {
	// DefaultRuntime is the runtime of the project's requests that name
	// none.
	DefaultRuntime string `json:"default_runtime,omitempty"`
	// StorageLimit bounds the bytes of the project's run evidence and
	// bundles; retention evicts its oldest evictable runs beyond it.
	StorageLimit int64 `json:"storage_limit,omitempty"`
	// RetentionRuns and RetentionAgeSeconds replace the server's retention
	// policy for the project's runs when either is set.
	RetentionRuns       int   `json:"retention_runs,omitempty"`
	RetentionAgeSeconds int64 `json:"retention_age_seconds,omitempty"`
}

// Projects is the projects the caller may see, in ID order, and the project
// set's revision, which every change names.
type Projects struct {
	Revision uint64    `json:"revision"`
	Projects []Project `json:"projects"`
}

// ProjectRequest creates or updates a project at the project set's
// revision. ID is ignored on an update, which names the project in its path.
type ProjectRequest struct {
	Revision uint64 `json:"revision"`
	ID       string `json:"id,omitempty"`
	Name     string `json:"name"`
	ProjectSettings
}

// ProjectRevision names the project set's revision for an archive or
// unarchive.
type ProjectRevision struct {
	Revision uint64 `json:"revision"`
}

// LibraryItem is a library artifact and the projects it is shared with: IDs
// or "*" alone. A caller who may not publish sees only its own projects of
// the audience and no sharer.
type LibraryItem struct {
	Artifact platform.ArtifactMetadata `json:"artifact"`
	Audience []string                  `json:"audience"`
	SharedBy string                    `json:"shared_by,omitempty"`
	Shared   *time.Time                `json:"shared,omitempty"`
}

// LibraryPage is one page of the library and the project set's revision.
type LibraryPage struct {
	Revision   uint64        `json:"revision"`
	Items      []LibraryItem `json:"items"`
	NextCursor string        `json:"next_cursor,omitempty"`
}

// SharingRequest sets the projects something is shared with, IDs or "*"
// alone, at the project set's revision.
type SharingRequest struct {
	Revision uint64   `json:"revision"`
	Projects []string `json:"projects"`
}

// Sharing is a sharing record after a change, with the project set's new
// revision.
type Sharing struct {
	Revision uint64    `json:"revision"`
	Projects []string  `json:"projects"`
	SetBy    string    `json:"set_by"`
	Set      time.Time `json:"set"`
}

// CopyRequest copies a revision of an artifact, its head when revision_id
// is empty, into the request's project. Label defaults to the source's.
type CopyRequest struct {
	RevisionID string `json:"revision_id,omitempty"`
	Label      string `json:"label,omitempty"`
}

// callerProject reads ?project=, which names the project a request works in.
func callerProject(r *http.Request) (string, error) {
	values := r.URL.Query()["project"]
	switch len(values) {
	case 0:
		return "", nil
	case 1:
		return values[0], nil
	}
	return "", errors.New("a request names at most one project: ?project=<id>")
}

func settingsOf(s platform.ProjectSettings) ProjectSettings {
	return ProjectSettings{DefaultRuntime: s.DefaultRuntime, StorageLimit: s.StorageLimit, RetentionRuns: s.RetentionRuns,
		RetentionAgeSeconds: int64(s.RetentionAge / time.Second)}
}

func (s ProjectSettings) platform() platform.ProjectSettings {
	return platform.ProjectSettings{DefaultRuntime: s.DefaultRuntime, StorageLimit: s.StorageLimit, RetentionRuns: s.RetentionRuns,
		RetentionAge: time.Duration(s.RetentionAgeSeconds) * time.Second}
}

func projectOf(p platform.Project) Project {
	perms := p.Permissions
	if perms == nil {
		perms = []string{}
	}
	return Project{ID: p.ID, Name: p.Name, BuiltIn: p.BuiltIn, Archived: p.Archived, ProjectSettings: settingsOf(p.ProjectSettings),
		Permissions: perms, CreatedBy: p.CreatedBy, Created: p.Created, UpdatedBy: p.UpdatedBy, Updated: p.Updated}
}

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	s.writeProjects(w, r, http.StatusOK)
}

// writeProjects answers with the projects the caller may see, so a change
// returns the revision the next one names.
func (s *Server) writeProjects(w http.ResponseWriter, r *http.Request, status int) {
	l, err := s.runs.ListProjects(r.Context(), callerOf(r))
	if err != nil {
		writeOperationError(w, err)
		return
	}
	out := Projects{Revision: l.Revision, Projects: make([]Project, 0, len(l.Projects))}
	for _, p := range l.Projects {
		out.Projects = append(out.Projects, projectOf(p))
	}
	writeJSON(w, status, out)
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	var req ProjectRequest
	if !decode(w, r, &req) {
		return
	}
	if _, err := s.runs.CreateProject(r.Context(), callerOf(r), platform.CreateProjectRequest{Revision: req.Revision, ID: req.ID, Name: req.Name,
		ProjectSettings: req.ProjectSettings.platform()}); err != nil {
		writeOperationError(w, err)
		return
	}
	s.writeProjects(w, r, http.StatusCreated)
}

func (s *Server) updateProject(w http.ResponseWriter, r *http.Request) {
	var req ProjectRequest
	if !decode(w, r, &req) {
		return
	}
	if _, err := s.runs.UpdateProject(r.Context(), callerOf(r), platform.UpdateProjectRequest{Revision: req.Revision, ID: r.PathValue("id"), Name: req.Name,
		ProjectSettings: req.ProjectSettings.platform()}); err != nil {
		writeOperationError(w, err)
		return
	}
	s.writeProjects(w, r, http.StatusOK)
}

func (s *Server) archiveProject(w http.ResponseWriter, r *http.Request) {
	s.setProjectArchived(w, r, true)
}

func (s *Server) unarchiveProject(w http.ResponseWriter, r *http.Request) {
	s.setProjectArchived(w, r, false)
}

func (s *Server) setProjectArchived(w http.ResponseWriter, r *http.Request, archived bool) {
	var req ProjectRevision
	if !decode(w, r, &req) {
		return
	}
	set := s.runs.UnarchiveProject
	if archived {
		set = s.runs.ArchiveProject
	}
	if _, err := set(r.Context(), callerOf(r), req.Revision, r.PathValue("id")); err != nil {
		writeOperationError(w, err)
		return
	}
	s.writeProjects(w, r, http.StatusOK)
}

func (s *Server) listLibrary(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	limit := 0
	if value := query.Get("limit"); value != "" {
		var err error
		if limit, err = strconv.Atoi(value); err != nil {
			writeArtifactFailure(w, http.StatusBadRequest, "invalid_request", invalidArtifactRequest)
			return
		}
	}
	page, err := s.runs.ListLibrary(r.Context(), callerOf(r), platform.ListLibraryRequest{Kind: platform.ArtifactKind(query.Get("kind")), Limit: limit, Cursor: query.Get("cursor")})
	if err != nil {
		writeArtifactError(w, err)
		return
	}
	out := LibraryPage{Revision: page.Revision, Items: make([]LibraryItem, 0, len(page.Items)), NextCursor: page.NextCursor}
	for _, item := range page.Items {
		out.Items = append(out.Items, LibraryItem{Artifact: item.Artifact, Audience: orEmpty(item.Audience), SharedBy: item.SharedBy, Shared: item.Shared})
	}
	writeJSON(w, http.StatusOK, out)
}

func sharingOf(v *platform.Sharing) Sharing {
	return Sharing{Revision: v.Revision, Projects: orEmpty(v.Projects), SetBy: v.SetBy, Set: v.Set}
}

func (s *Server) setLibraryAudience(w http.ResponseWriter, r *http.Request) {
	var req SharingRequest
	if !decode(w, r, &req) {
		return
	}
	v, err := s.runs.SetLibraryAudience(r.Context(), callerOf(r), platform.SetLibraryAudienceRequest{Revision: req.Revision, ArtifactID: r.PathValue("artifact"), Projects: req.Projects})
	if err != nil {
		writeArtifactError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sharingOf(v))
}

func (s *Server) setRuntimeAvailability(w http.ResponseWriter, r *http.Request) {
	var req SharingRequest
	if !decode(w, r, &req) {
		return
	}
	v, err := s.runs.SetRuntimeAvailability(r.Context(), callerOf(r), platform.SetRuntimeAvailabilityRequest{Revision: req.Revision, Runtime: r.PathValue("name"), Projects: req.Projects})
	if err != nil {
		writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sharingOf(v))
}

func (s *Server) copyArtifact(w http.ResponseWriter, r *http.Request) {
	var req CopyRequest
	if !decode(w, r, &req) {
		return
	}
	saved, err := s.runs.CopyArtifact(r.Context(), callerOf(r), platform.CopyArtifactRequest{ArtifactID: r.PathValue("artifact"), RevisionID: req.RevisionID, Label: req.Label})
	if err != nil {
		writeArtifactError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, saved)
}

func (s *Server) routeProjects() {
	s.mux.HandleFunc("GET /api/v1/projects", s.listProjects)
	s.mux.HandleFunc("POST /api/v1/projects", s.createProject)
	s.mux.HandleFunc("PUT /api/v1/projects/{id}", s.updateProject)
	s.mux.HandleFunc("POST /api/v1/projects/{id}/archive", s.archiveProject)
	s.mux.HandleFunc("POST /api/v1/projects/{id}/unarchive", s.unarchiveProject)
	s.mux.HandleFunc("GET /api/v1/library", s.listLibrary)
	s.mux.HandleFunc("PUT /api/v1/library/{artifact}/audience", s.setLibraryAudience)
	s.mux.HandleFunc("POST /api/v1/artifacts/{artifact}/copies", s.copyArtifact)
	s.mux.HandleFunc("PUT /api/v1/runtimes/{name}/availability", s.setRuntimeAvailability)
}

// projectParam is the query parameter every operation takes.
var projectParam = map[string]any{"name": "project", "in": "query", "schema": map[string]any{"type": "string"},
	"description": "The project the request works in: the project new work and saved artifacts go to, whose runtimes and saved work it uses, and that narrows lists. Empty is the default project; operations on an existing resource use the resource's project."}

func projectPaths() map[string]any {
	changed := func(desc string) map[string]any { return reply(desc, ref("Projects")) }
	artifactParam := map[string]any{"name": "artifact", "in": "path", "required": true, "schema": map[string]any{"type": "string"}}
	return map[string]any{
		"/api/v1/projects": map[string]any{
			"get": op("listProjects",
				"The projects in which the caller holds a permission, with those permissions, and the library; project.manage holders see every project.",
				nil, "", map[string]any{
					"200": changed("The projects and the project set's revision."),
					"503": operationFailure("The server keeps no saved work."),
				}),
			"post": op("createProject",
				"Create a project (project.manage). Its ID is 1 to 64 lowercase letters, digits and hyphens; default and library are built in.",
				nil, "ProjectRequest", map[string]any{
					"201": changed("The projects after the change."),
					"400": operationFailure("An invalid ID, name or setting."),
					"403": operationFailure("The caller lacks project.manage."),
					"409": operationFailure("The revision is stale, or the project exists."),
					"413": operationFailure("The server holds the most projects it keeps."),
				}),
		},
		"/api/v1/projects/{id}": map[string]any{
			"put": op("updateProject", "Replace a project's name and settings (project.manage).",
				[]any{idParam}, "ProjectRequest", map[string]any{
					"200": changed("The projects after the change."),
					"400": operationFailure("An invalid name or setting."),
					"403": operationFailure("The caller lacks project.manage."),
					"404": operationFailure("Unknown project."),
					"409": operationFailure("The revision is stale."),
				}),
		},
		"/api/v1/projects/{id}/archive": map[string]any{
			"post": op("archiveProject",
				"Archive a project (project.manage): its work stays readable and deletable, and it takes no new runs, suites, saves or captures. The built-in projects cannot be archived.",
				[]any{idParam}, "ProjectRevision", map[string]any{
					"200": changed("The projects after the change."),
					"403": operationFailure("The caller lacks project.manage, or the project is built in."),
					"404": operationFailure("Unknown project."),
					"409": operationFailure("The revision is stale."),
				}),
		},
		"/api/v1/projects/{id}/unarchive": map[string]any{
			"post": op("unarchiveProject", "Let an archived project take new work again (project.manage).",
				[]any{idParam}, "ProjectRevision", map[string]any{
					"200": changed("The projects after the change."),
					"403": operationFailure("The caller lacks project.manage, or the project is built in."),
					"404": operationFailure("Unknown project."),
					"409": operationFailure("The revision is stale."),
				}),
		},
		"/api/v1/library": map[string]any{
			"get": op("listLibrary",
				"The library's artifacts and the projects each is shared with. Callers who may read the library see everything; others see the items shared with a project in which they may read artifacts. ?project= narrows it to the items shared with that project.",
				[]any{
					map[string]any{"name": "kind", "in": "query", "schema": map[string]any{"type": "string", "enum": []string{"configuration", "fixture", "scenario"}}},
					map[string]any{"name": "limit", "in": "query", "schema": map[string]any{"type": "integer"}},
					map[string]any{"name": "cursor", "in": "query", "schema": map[string]any{"type": "string"}},
				}, "", map[string]any{
					"200": reply("A page of the library.", ref("LibraryPage")),
					"400": reply("Invalid kind, limit or cursor.", ref("ArtifactError")),
					"403": reply("The caller may read no artifacts.", ref("ArtifactError")),
				}),
		},
		"/api/v1/library/{artifact}/audience": map[string]any{
			"put": op("setLibraryAudience",
				"Share a library artifact with projects (library.publish): project IDs, \"*\" alone for every project, or none. Work in a project of the audience may read and use the artifact, and copy it.",
				[]any{artifactParam}, "SharingRequest", map[string]any{
					"200": reply("The audience after the change.", ref("Sharing")),
					"400": reply("The artifact is not in the library, or an unknown project.", ref("ArtifactError")),
					"403": reply("The caller lacks library.publish.", ref("ArtifactError")),
					"404": reply("Unknown artifact.", ref("ArtifactError")),
					"409": reply("The revision is stale.", ref("ArtifactError")),
				}),
		},
		"/api/v1/artifacts/{artifact}/copies": map[string]any{
			"post": op("copyArtifact",
				"Copy a revision of an artifact the caller can read into the request's project as a new artifact that keeps the revision's provenance. Copying into the library publishes it there; copying out of it uses an item shared with the project.",
				[]any{artifactParam}, "CopyRequest", map[string]any{
					"201": reply("The copy.", ref("Saved")),
					"400": reply("Invalid request.", ref("ArtifactError")),
					"403": reply("The caller may not write in the project or read the source.", ref("ArtifactError")),
					"404": reply("Unknown artifact or revision.", ref("ArtifactError")),
					"409": reply("The project is archived.", ref("ArtifactError")),
				}),
		},
		"/api/v1/runtimes/{name}/availability": map[string]any{
			"put": op("setRuntimeAvailability",
				"Limit the projects a runtime runs work for (runtime.manage): project IDs or \"*\" alone; an empty list makes it available to every project again. Work in another project that names the runtime is refused.",
				[]any{map[string]any{"name": "name", "in": "path", "required": true, "schema": map[string]any{"type": "string"}}}, "SharingRequest", map[string]any{
					"200": reply("The availability after the change.", ref("Sharing")),
					"400": operationFailure("An invalid runtime name or an unknown project."),
					"403": operationFailure("The caller lacks runtime.manage."),
					"409": operationFailure("The revision is stale."),
				}),
		},
	}
}
