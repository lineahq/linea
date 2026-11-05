package pg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lineahq/linea/internal/debugger/artifact"
)

// The project set is outside StorageBytes, like role bindings; its bounds
// are artifact.MaxProjects, MaxAudiences and MaxAvailabilities. Writes run
// under the family's advisory lock, so the first write is ordered too.

// ProjectSet returns the project set and its revision.
func (s *Store) ProjectSet(ctx context.Context) (artifact.ProjectSet, error) {
	if err := s.ready(ctx); err != nil {
		return artifact.ProjectSet{}, err
	}
	var set artifact.ProjectSet
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var err error
		set, err = readProjects(ctx, tx)
		return err
	})
	return set, storageErr("projects", err)
}

// SaveProject records a project at revision expected.
func (s *Store) SaveProject(ctx context.Context, expected uint64, p artifact.ProjectRecord) (artifact.ProjectSet, error) {
	if err := artifact.ValidateProject(p); err != nil {
		return artifact.ProjectSet{}, err
	}
	if err := s.ready(ctx); err != nil {
		return artifact.ProjectSet{}, err
	}
	return s.writeProjects(ctx, "save project", expected, func(tx pgx.Tx, set artifact.ProjectSet) error {
		if _, ok := set.Project(p.ID); !ok && len(set.Projects) >= artifact.MaxProjects {
			return artifact.ErrLimit
		}
		var updated *time.Time
		if !p.Updated.IsZero() {
			at := p.Updated.UTC()
			updated = &at
		}
		_, err := tx.Exec(ctx, `INSERT INTO projects (id, name, archived, default_runtime, storage_limit, retention_runs, retention_age, created_by, created_at, updated_by, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
			ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, archived = EXCLUDED.archived, default_runtime = EXCLUDED.default_runtime,
				storage_limit = EXCLUDED.storage_limit, retention_runs = EXCLUDED.retention_runs, retention_age = EXCLUDED.retention_age,
				created_by = EXCLUDED.created_by, created_at = EXCLUDED.created_at, updated_by = EXCLUDED.updated_by, updated_at = EXCLUDED.updated_at`,
			p.ID, p.Name, p.Archived, p.DefaultRuntime, p.StorageLimit, p.RetentionRuns, int64(p.RetentionAge), p.CreatedBy, p.Created.UTC(), p.UpdatedBy, updated)
		return err
	})
}

// SetAudience records a library artifact's audience at revision expected.
func (s *Store) SetAudience(ctx context.Context, expected uint64, a artifact.AudienceRecord) (artifact.ProjectSet, error) {
	if err := artifact.ValidateAudience(a); err != nil {
		return artifact.ProjectSet{}, err
	}
	if err := s.ready(ctx); err != nil {
		return artifact.ProjectSet{}, err
	}
	return s.writeProjects(ctx, "set audience", expected, func(tx pgx.Tx, set artifact.ProjectSet) error {
		_, exists := set.Audience(a.ArtifactID)
		return writeProjectList(ctx, tx, audienceTable, a.ArtifactID, a.Projects, a.SetBy, a.Set, exists, len(set.Audiences), artifact.MaxAudiences)
	})
}

// SetAvailability records a runtime's availability at revision expected.
func (s *Store) SetAvailability(ctx context.Context, expected uint64, a artifact.AvailabilityRecord) (artifact.ProjectSet, error) {
	if err := artifact.ValidateAvailability(a); err != nil {
		return artifact.ProjectSet{}, err
	}
	if err := s.ready(ctx); err != nil {
		return artifact.ProjectSet{}, err
	}
	return s.writeProjects(ctx, "set availability", expected, func(tx pgx.Tx, set artifact.ProjectSet) error {
		_, exists := set.Availability(a.Runtime)
		return writeProjectList(ctx, tx, availabilityTable, a.Runtime, a.Projects, a.SetBy, a.Set, exists, len(set.Availabilities), artifact.MaxAvailabilities)
	})
}

// listTable is a table of project lists keyed by one text column.
type listTable struct{ name, key string }

var (
	audienceTable     = listTable{"library_audiences", "artifact_id"}
	availabilityTable = listTable{"runtime_availability", "runtime"}
)

// writeProjectList upserts the row of key in t, or deletes it for an empty
// list.
func writeProjectList(ctx context.Context, tx pgx.Tx, t listTable, key string, projects []string, by string, at time.Time, exists bool, count, max int) error {
	if len(projects) == 0 {
		_, err := tx.Exec(ctx, "DELETE FROM "+t.name+" WHERE "+t.key+" = $1", key)
		return err
	}
	if !exists && count >= max {
		return artifact.ErrLimit
	}
	_, err := tx.Exec(ctx, "INSERT INTO "+t.name+" ("+t.key+", projects, set_by, set_at) VALUES ($1, $2, $3, $4) "+
		"ON CONFLICT ("+t.key+") DO UPDATE SET projects = EXCLUDED.projects, set_by = EXCLUDED.set_by, set_at = EXCLUDED.set_at",
		key, projects, by, at.UTC())
	return err
}

// writeProjects runs write on the set at revision expected and raises the
// revision, which records the change.
func (s *Store) writeProjects(ctx context.Context, op string, expected uint64, write func(pgx.Tx, artifact.ProjectSet) error) (artifact.ProjectSet, error) {
	var out artifact.ProjectSet
	err := s.inTx(ctx, op, func(tx pgx.Tx) error {
		if err := lockFamily(ctx, tx, "projects"); err != nil {
			return err
		}
		set, err := readProjects(ctx, tx)
		if err != nil {
			return err
		}
		if set.Revision != expected {
			return fmt.Errorf("%w: projects are at revision %d, not %d", artifact.ErrConflict, set.Revision, expected)
		}
		if err := write(tx, set); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO project_set (id, revision) VALUES ('projects', 1)
			ON CONFLICT (id) DO UPDATE SET revision = project_set.revision + 1`); err != nil {
			return err
		}
		out, err = readProjects(ctx, tx)
		return err
	})
	if err != nil {
		return artifact.ProjectSet{}, err
	}
	return out, nil
}

func readProjects(ctx context.Context, tx pgx.Tx) (artifact.ProjectSet, error) {
	var set artifact.ProjectSet
	var revision int64
	if err := tx.QueryRow(ctx, "SELECT revision FROM project_set").Scan(&revision); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return set, err
	}
	set.Revision = uint64(revision)
	projects, err := readProjectRows(ctx, tx)
	if err != nil {
		return set, err
	}
	set.Projects = projects
	audiences, err := readProjectLists(ctx, tx, audienceTable)
	if err != nil {
		return set, err
	}
	for _, l := range audiences {
		set.Audiences = append(set.Audiences, artifact.AudienceRecord{ArtifactID: l.key, Projects: l.projects, SetBy: l.by, Set: l.at})
	}
	availabilities, err := readProjectLists(ctx, tx, availabilityTable)
	if err != nil {
		return set, err
	}
	for _, l := range availabilities {
		set.Availabilities = append(set.Availabilities, artifact.AvailabilityRecord{Runtime: l.key, Projects: l.projects, SetBy: l.by, Set: l.at})
	}
	return set, nil
}

func readProjectRows(ctx context.Context, tx pgx.Tx) ([]artifact.ProjectRecord, error) {
	rows, err := tx.Query(ctx, `SELECT id, name, archived, default_runtime, storage_limit, retention_runs, retention_age, created_by, created_at, updated_by, updated_at
		FROM projects ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []artifact.ProjectRecord
	for rows.Next() {
		var p artifact.ProjectRecord
		var age int64
		var updated *time.Time
		if err := rows.Scan(&p.ID, &p.Name, &p.Archived, &p.DefaultRuntime, &p.StorageLimit, &p.RetentionRuns, &age, &p.CreatedBy, &p.Created, &p.UpdatedBy, &updated); err != nil {
			return nil, err
		}
		p.RetentionAge, p.Created = time.Duration(age), p.Created.UTC()
		if updated != nil {
			p.Updated = updated.UTC()
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

type projectList struct {
	key, by  string
	projects []string
	at       time.Time
}

func readProjectLists(ctx context.Context, tx pgx.Tx, t listTable) ([]projectList, error) {
	rows, err := tx.Query(ctx, "SELECT "+t.key+", projects, set_by, set_at FROM "+t.name+" ORDER BY "+t.key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []projectList
	for rows.Next() {
		var l projectList
		if err := rows.Scan(&l.key, &l.projects, &l.by, &l.at); err != nil {
			return nil, err
		}
		l.at = l.at.UTC()
		out = append(out, l)
	}
	return out, rows.Err()
}
