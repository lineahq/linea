package pg

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/lineahq/linea/internal/debugger/artifact"
)

func validOrigin(o *artifact.Origin) bool {
	if o == nil {
		return true
	}
	if !isLowerHex(o.BaseDigest, 64) {
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

const artifactColumns = "id, project_id, kind, label, generation, head_revision_id, created_at, updated_at"

func (s *Store) scanArtifact(row pgx.Row) (artifact.Metadata, error) {
	var a artifact.Metadata
	var kind string
	var generation int64
	var label []byte
	err := row.Scan(&a.ID, &a.Project, &kind, &label, &generation, &a.Head, &a.Created, &a.Updated)
	a.WorkspaceID, a.Kind, a.Generation, a.Label = s.id, artifact.Kind(kind), uint64(generation), string(label)
	a.Created, a.Updated = a.Created.UTC(), a.Updated.UTC()
	return a, err
}

// Save publishes a revision of a new or existing artifact. The installation
// lock serializes it with every other write that adds bytes, so the revision
// and storage limits and the expected generation hold across instances.
func (s *Store) Save(ctx context.Context, req artifact.SaveRequest) (*artifact.Saved, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	if !validID(req.WorkspaceID) {
		return nil, invalid("workspace_id is required")
	}
	if req.WorkspaceID != s.id {
		return nil, &artifact.Conflict{}
	}
	if !req.Kind.Valid() || len(req.Label) > 1024 || req.Actor == "" || len(req.Actor) > 256 || !validText(req.Actor) {
		return nil, invalid("kind, label or trusted actor is invalid")
	}
	if !validOrigin(req.Origin) {
		return nil, invalid("origin is invalid")
	}
	if int64(len(req.Content)) > s.limits.BlobBytes {
		return nil, artifact.ErrLimit
	}
	var saved *artifact.Saved
	err := s.inTx(ctx, "save", func(tx pgx.Tx) error {
		inst, err := lockInstallation(ctx, tx)
		if err != nil {
			return err
		}
		if inst.generation >= int64(s.limits.Revisions) {
			return artifact.ErrLimit
		}
		at := now()
		a := artifact.Metadata{WorkspaceID: s.id, ID: newID(), Project: req.Project, Kind: req.Kind, Label: req.Label, Created: at}
		creating := req.ArtifactID == ""
		if creating {
			if req.ExpectedGeneration != 0 || req.ExpectedHead != "" {
				return invalid("a new artifact cannot have an expected head")
			}
			if !artifact.ValidProject(req.Project) {
				return invalid("a new artifact needs a valid project")
			}
		} else {
			old, err := s.scanArtifact(tx.QueryRow(ctx, "SELECT "+artifactColumns+" FROM artifacts WHERE id = $1 FOR UPDATE", req.ArtifactID))
			if errors.Is(err, pgx.ErrNoRows) {
				return artifact.ErrNotFound
			}
			if err != nil {
				return err
			}
			if req.ExpectedGeneration == 0 || req.ExpectedHead == "" {
				return invalid("updates require expected generation and head")
			}
			if req.ExpectedGeneration != old.Generation || req.ExpectedHead != old.Head {
				return &artifact.Conflict{Current: &old}
			}
			if req.Kind != old.Kind {
				return invalid("artifact kind cannot change")
			}
			if req.Project != "" && req.Project != old.Project {
				return invalid("artifact project cannot change")
			}
			a = old
			a.Label = req.Label
		}
		r := artifact.Revision{Ref: artifact.Ref{WorkspaceID: s.id, ArtifactID: a.ID, RevisionID: newID()}, Generation: a.Generation + 1, Parent: a.Head,
			Digest: digestOf(req.Content), Bytes: int64(len(req.Content)), Created: at, Actor: req.Actor, Origin: cloneOrigin(req.Origin)}
		a.Generation, a.Head, a.Updated = r.Generation, r.Ref.RevisionID, at
		additional, err := newBytes(ctx, tx, blob{r.Digest, r.Bytes})
		if err != nil {
			return err
		}
		if err := s.checkSpace(ctx, tx, additional, reserve{}); err != nil {
			return err
		}
		var origin []byte
		var originRun *string
		if r.Origin != nil {
			origin, _ = json.Marshal(r.Origin)
			originRun = nullable(r.Origin.RunID)
		}
		// The object is stored before the row that names it commits.
		if err := s.putBlob(ctx, r.Digest, req.Content); err != nil {
			return err
		}
		if creating {
			_, err = tx.Exec(ctx, "INSERT INTO artifacts ("+artifactColumns+") VALUES ($1, $2, $3, $4, $5, $6, $7, $8)",
				a.ID, a.Project, string(a.Kind), []byte(a.Label), int64(a.Generation), a.Head, a.Created, a.Updated)
		} else {
			_, err = tx.Exec(ctx, "UPDATE artifacts SET label = $2, generation = $3, head_revision_id = $4, updated_at = $5 WHERE id = $1",
				a.ID, []byte(a.Label), int64(a.Generation), a.Head, a.Updated)
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO revisions (id, artifact_id, generation, parent_revision_id, digest, bytes, created_at, actor, origin, origin_run)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`, r.Ref.RevisionID, a.ID, int64(r.Generation), r.Parent, r.Digest, r.Bytes, r.Created, r.Actor, origin, originRun); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "UPDATE installation SET generation = generation + 1"); err != nil {
			return err
		}
		saved = &artifact.Saved{Artifact: a, Revision: r}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return saved, nil
}

// Get returns an artifact's metadata.
func (s *Store) Get(ctx context.Context, id string) (artifact.Metadata, error) {
	if err := s.ready(ctx); err != nil {
		return artifact.Metadata{}, err
	}
	a, err := s.scanArtifact(s.pool.QueryRow(ctx, "SELECT "+artifactColumns+" FROM artifacts WHERE id = $1", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return artifact.Metadata{}, artifact.ErrNotFound
	}
	return a, storageErr("get", err)
}

const revisionColumns = "id, artifact_id, generation, parent_revision_id, digest, bytes, created_at, actor, origin"

func (s *Store) scanRevision(row pgx.Row) (artifact.Revision, error) {
	var r artifact.Revision
	var generation int64
	var origin []byte
	err := row.Scan(&r.Ref.RevisionID, &r.Ref.ArtifactID, &generation, &r.Parent, &r.Digest, &r.Bytes, &r.Created, &r.Actor, &origin)
	if err != nil {
		return r, err
	}
	r.Ref.WorkspaceID, r.Generation, r.Created = s.id, uint64(generation), r.Created.UTC()
	if len(origin) != 0 {
		r.Origin = new(artifact.Origin)
		if err := json.Unmarshal(origin, r.Origin); err != nil {
			return r, err
		}
	}
	return r, nil
}

// Revision returns one revision with its exact content.
func (s *Store) Revision(ctx context.Context, ref artifact.Ref) (*artifact.RevisionData, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	if ref.WorkspaceID != s.id {
		return nil, artifact.ErrNotFound
	}
	r, err := s.scanRevision(s.pool.QueryRow(ctx, "SELECT "+revisionColumns+" FROM revisions WHERE id = $1 AND artifact_id = $2", ref.RevisionID, ref.ArtifactID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, artifact.ErrNotFound
	}
	if err != nil {
		return nil, storageErr("revision", err)
	}
	raw, err := s.readBlob(ctx, r.Digest, r.Bytes)
	if err != nil {
		return nil, err
	}
	return &artifact.RevisionData{Revision: r, Content: raw}, nil
}

type revisionCursor struct {
	Workspace string `json:"workspace"`
	Artifact  string `json:"artifact"`
	Before    uint64 `json:"before"`
}

// Revisions lists an artifact's revisions newest first. Revisions are only
// ever appended, so a cursor names the generation the next page starts below
// and stays valid while the artifact gains revisions.
func (s *Store) Revisions(ctx context.Context, req artifact.RevisionsRequest) (*artifact.RevisionPage, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	if req.Limit == 0 {
		req.Limit = 25
	}
	if req.Limit < 1 || req.Limit > 100 {
		return nil, invalid("page limit must be between 1 and 100")
	}
	a, err := s.Get(ctx, req.ArtifactID)
	if err != nil {
		return nil, err
	}
	c := revisionCursor{Workspace: s.id, Artifact: a.ID, Before: a.Generation + 1}
	if req.Cursor != "" {
		var raw []byte
		raw, err = base64.RawURLEncoding.DecodeString(req.Cursor)
		if len(req.Cursor) > 1024 || err != nil || decodeStrict(raw, &c) != nil || c.Workspace != s.id || c.Artifact != a.ID || c.Before < 2 || c.Before > a.Generation {
			return nil, invalid("invalid revision cursor")
		}
	}
	rows, err := s.pool.Query(ctx, "SELECT "+revisionColumns+" FROM revisions WHERE artifact_id = $1 AND generation < $2 ORDER BY generation DESC LIMIT $3",
		a.ID, int64(c.Before), req.Limit+1)
	if err != nil {
		return nil, storageErr("revisions", err)
	}
	defer rows.Close()
	items := []artifact.Revision{}
	for rows.Next() {
		r, err := s.scanRevision(rows)
		if err != nil {
			return nil, storageErr("revisions", err)
		}
		items = append(items, r)
	}
	if err := rows.Err(); err != nil {
		return nil, storageErr("revisions", err)
	}
	p := &artifact.RevisionPage{Artifact: a, Items: items[:min(len(items), req.Limit)]}
	if len(items) > req.Limit {
		c.Before = p.Items[req.Limit-1].Generation
		raw, _ := json.Marshal(c)
		p.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return p, nil
}

type listCursor struct {
	Workspace string        `json:"workspace"`
	After     string        `json:"after"`
	Kind      artifact.Kind `json:"kind"`
	// Projects is the request's project filter, which a continuation must
	// repeat.
	Projects []string `json:"projects,omitempty"`
}

// List pages artifact metadata by ID. A cursor names the last ID returned, so
// artifacts created meanwhile never disturb a walk.
func (s *Store) List(ctx context.Context, req artifact.ListRequest) (*artifact.Page, error) {
	if err := s.ready(ctx); err != nil {
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
	c := listCursor{Workspace: s.id, Kind: req.Kind, Projects: req.Projects}
	if req.Cursor != "" {
		if len(req.Cursor) > 1024 {
			return nil, invalid("invalid list cursor")
		}
		raw, err := base64.RawURLEncoding.DecodeString(req.Cursor)
		if err != nil || decodeStrict(raw, &c) != nil || c.Workspace != s.id || c.Kind != req.Kind || !slices.Equal(c.Projects, req.Projects) || !validID(c.After) {
			return nil, invalid("invalid list cursor")
		}
	}
	rows, err := s.pool.Query(ctx, `SELECT `+artifactColumns+` FROM artifacts
		WHERE id > $1 AND ($2 = '' OR kind = $2) AND ($3::text[] IS NULL OR project_id = ANY($3))
		ORDER BY id LIMIT $4`, c.After, string(req.Kind), req.Projects, req.Limit+1)
	if err != nil {
		return nil, storageErr("list", err)
	}
	defer rows.Close()
	items := []artifact.Metadata{}
	for rows.Next() {
		a, err := s.scanArtifact(rows)
		if err != nil {
			return nil, storageErr("list", err)
		}
		items = append(items, a)
	}
	if err := rows.Err(); err != nil {
		return nil, storageErr("list", err)
	}
	rows.Close()
	p := &artifact.Page{Workspace: s.Info(), Items: items[:min(len(items), req.Limit)]}
	if len(items) > req.Limit {
		c.After = p.Items[req.Limit-1].ID
		raw, _ := json.Marshal(c)
		p.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return p, nil
}

func decodeStrict(raw []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}
