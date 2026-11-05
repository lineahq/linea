package memory

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
)

func validOrigin(o *artifact.Origin) bool {
	if o == nil {
		return true
	}
	d, err := hex.DecodeString(o.BaseDigest)
	if err != nil || len(d) != sha256.Size || o.BaseDigest != strings.ToLower(o.BaseDigest) {
		return false
	}
	switch o.Kind {
	case "candidate":
		return o.RunID == "" && (o.BaseRevision == "" || validID(o.BaseRevision)) && len(o.Edit) <= 8<<10 && (len(o.Edit) == 0 || json.Valid(o.Edit))
	case "run":
		return validRunID(o.RunID) && o.BaseRevision == "" && len(o.Edit) == 0
	case "capture":
		return o.RunID == "" && o.BaseRevision == "" && len(o.Edit) > 0 && len(o.Edit) <= 128<<10 && json.Valid(o.Edit)
	default:
		return false
	}
}

func cloneOrigin(o *artifact.Origin) *artifact.Origin {
	if o == nil {
		return nil
	}
	c := *o
	c.Edit = append(json.RawMessage(nil), o.Edit...)
	return &c
}

// Save publishes a revision and its artifact head together.
func (s *Store) Save(ctx context.Context, req artifact.SaveRequest) (*artifact.Saved, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return nil, err
	}
	if !validID(req.WorkspaceID) {
		return nil, invalid("workspace_id is required")
	}
	if req.WorkspaceID != d.cat.Workspace.ID {
		return nil, &artifact.Conflict{}
	}
	if !req.Kind.Valid() || len(req.Label) > 1024 || req.Actor == "" || len(req.Actor) > 256 {
		return nil, invalid("kind, label or trusted actor is invalid")
	}
	if !validOrigin(req.Origin) {
		return nil, invalid("origin is invalid")
	}
	if int64(len(req.Content)) > s.limits.BlobBytes || len(d.cat.Revisions) >= s.limits.Revisions {
		return nil, artifact.ErrLimit
	}
	now := time.Now().UTC()
	a := artifact.Metadata{WorkspaceID: d.cat.Workspace.ID, ID: newID(), Project: req.Project, Kind: req.Kind, Label: req.Label, Created: now}
	if req.ArtifactID == "" {
		if req.ExpectedGeneration != 0 || req.ExpectedHead != "" {
			return nil, invalid("a new artifact cannot have an expected head")
		}
		if !artifact.ValidProject(req.Project) {
			return nil, invalid("a new artifact needs a valid project")
		}
	} else {
		old, ok := d.cat.Artifacts[req.ArtifactID]
		if !ok {
			return nil, artifact.ErrNotFound
		}
		if req.ExpectedGeneration == 0 || req.ExpectedHead == "" {
			return nil, invalid("updates require expected generation and head")
		}
		if req.ExpectedGeneration != old.Generation || req.ExpectedHead != old.Head {
			return nil, &artifact.Conflict{Current: &old}
		}
		if req.Kind != old.Kind {
			return nil, invalid("artifact kind cannot change")
		}
		if req.Project != "" && req.Project != old.Project {
			return nil, invalid("artifact project cannot change")
		}
		a = old
		a.Label = req.Label
	}
	r := artifact.Revision{Ref: artifact.Ref{WorkspaceID: d.cat.Workspace.ID, ArtifactID: a.ID, RevisionID: newID()}, Generation: a.Generation + 1,
		Parent: a.Head, Digest: digest(req.Content), Bytes: int64(len(req.Content)), Created: now, Actor: req.Actor, Origin: cloneOrigin(req.Origin)}
	a.Generation, a.Head, a.Updated = r.Generation, r.Ref.RevisionID, now
	next := d.cat
	next.Artifacts = make(map[string]artifact.Metadata, len(d.cat.Artifacts)+1)
	next.Revisions = make(map[string]artifact.Revision, len(d.cat.Revisions)+1)
	for id, value := range d.cat.Artifacts {
		next.Artifacts[id] = value
	}
	for id, value := range d.cat.Revisions {
		next.Revisions[id] = value
	}
	next.Workspace.Generation++
	next.Workspace.Schema = artifact.SchemaVersion
	next.Artifacts[a.ID], next.Revisions[r.Ref.RevisionID] = a, r
	size := encodedSize(next)
	if size > s.limits.CatalogBytes {
		return nil, artifact.ErrLimit
	}
	additional := size
	if _, exists := d.blobs[r.Digest]; !exists {
		additional += r.Bytes
	}
	used, err := s.usage()
	if err != nil {
		return nil, err
	}
	// The new catalog is counted on top of the old one, a margin that errs on
	// the side of refusing.
	if err := s.checkSpace(used, additional, s.reserved(d.runs)); err != nil {
		return nil, artifact.ErrLimit
	}
	d.storeBlob(r.Digest, req.Content)
	d.cat, d.catSize = next, size
	r.Origin = cloneOrigin(r.Origin)
	return &artifact.Saved{Artifact: a, Revision: r}, nil
}

// Get returns an artifact's current metadata.
func (s *Store) Get(ctx context.Context, id string) (artifact.Metadata, error) {
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return artifact.Metadata{}, err
	}
	a, ok := s.d.cat.Artifacts[id]
	if !ok {
		return artifact.Metadata{}, artifact.ErrNotFound
	}
	return a, nil
}

// Revision returns one revision with its content.
func (s *Store) Revision(ctx context.Context, ref artifact.Ref) (*artifact.RevisionData, error) {
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return nil, err
	}
	r, ok := s.d.cat.Revisions[ref.RevisionID]
	if !ok || r.Ref != ref {
		return nil, artifact.ErrNotFound
	}
	r.Origin = cloneOrigin(r.Origin)
	return &artifact.RevisionData{Revision: r, Content: cloneBytes(s.d.blobs[r.Digest])}, nil
}

type listCursor struct {
	Workspace  string        `json:"workspace"`
	Generation uint64        `json:"generation"`
	After      string        `json:"after"`
	Kind       artifact.Kind `json:"kind"`
	Projects   []string      `json:"projects,omitempty"`
}

func inProjects(filter []string, project string) bool {
	return filter == nil || slices.Contains(filter, project)
}

// List pages artifact metadata by ID. A changed catalog invalidates a
// continuation rather than mixing heads from different generations.
func (s *Store) List(ctx context.Context, req artifact.ListRequest) (*artifact.Page, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return nil, err
	}
	if req.Kind != "" && !req.Kind.Valid() {
		return nil, invalid("unknown artifact kind")
	}
	if req.Limit == 0 {
		req.Limit = 25
	}
	if req.Limit < 1 || req.Limit > 100 {
		return nil, invalid("page limit must be between 1 and 100")
	}
	if req.Projects != nil {
		// An empty filter lists nothing; keep it distinct from nil.
		req.Projects = append([]string{}, slices.Compact(slices.Sorted(slices.Values(req.Projects)))...)
	}
	c := listCursor{Workspace: d.cat.Workspace.ID, Generation: d.cat.Workspace.Generation, Kind: req.Kind, Projects: req.Projects}
	if req.Cursor != "" {
		if len(req.Cursor) > 1024 {
			return nil, invalid("invalid list cursor")
		}
		raw, err := base64.RawURLEncoding.DecodeString(req.Cursor)
		if err != nil || decodeStrict(raw, &c) != nil || c.Workspace != d.cat.Workspace.ID || c.Kind != req.Kind || !slices.Equal(c.Projects, req.Projects) || !validID(c.After) {
			return nil, invalid("invalid list cursor")
		}
		if c.Generation != d.cat.Workspace.Generation {
			return nil, &artifact.Conflict{}
		}
		if _, ok := d.cat.Artifacts[c.After]; !ok {
			return nil, invalid("invalid list cursor")
		}
	}
	ids := make([]string, 0, len(d.cat.Artifacts))
	for id, a := range d.cat.Artifacts {
		if id > c.After && (req.Kind == "" || a.Kind == req.Kind) && inProjects(req.Projects, a.Project) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	p := &artifact.Page{Workspace: d.cat.Workspace, Items: []artifact.Metadata{}}
	for _, id := range ids[:min(len(ids), req.Limit)] {
		p.Items = append(p.Items, d.cat.Artifacts[id])
	}
	if len(ids) > req.Limit {
		c.After = ids[req.Limit-1]
		raw, _ := json.Marshal(c)
		p.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return p, nil
}

type revisionCursor struct {
	Workspace string `json:"workspace"`
	Artifact  string `json:"artifact"`
	Before    uint64 `json:"before"`
}

// Revisions lists an artifact's revisions newest first. A cursor names the
// generation the next page starts below and stays valid while the artifact
// gains revisions.
func (s *Store) Revisions(ctx context.Context, req artifact.RevisionsRequest) (*artifact.RevisionPage, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return nil, err
	}
	if req.Limit == 0 {
		req.Limit = 25
	}
	if req.Limit < 1 || req.Limit > 100 {
		return nil, invalid("page limit must be between 1 and 100")
	}
	a, ok := d.cat.Artifacts[req.ArtifactID]
	if !ok {
		return nil, artifact.ErrNotFound
	}
	c := revisionCursor{Workspace: d.cat.Workspace.ID, Artifact: a.ID, Before: a.Generation + 1}
	if req.Cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(req.Cursor)
		if len(req.Cursor) > 1024 || err != nil || decodeStrict(raw, &c) != nil || c.Workspace != d.cat.Workspace.ID || c.Artifact != a.ID || c.Before < 2 || c.Before > a.Generation {
			return nil, invalid("invalid revision cursor")
		}
	}
	items := []artifact.Revision{}
	for _, r := range d.cat.Revisions {
		if r.Ref.ArtifactID == a.ID && r.Generation < c.Before {
			r.Origin = cloneOrigin(r.Origin)
			items = append(items, r)
		}
	}
	slices.SortFunc(items, func(x, y artifact.Revision) int { return cmp.Compare(y.Generation, x.Generation) })
	p := &artifact.RevisionPage{Artifact: a, Items: items[:min(len(items), req.Limit)]}
	if len(items) > req.Limit {
		c.Before = p.Items[req.Limit-1].Generation
		raw, _ := json.Marshal(c)
		p.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return p, nil
}
