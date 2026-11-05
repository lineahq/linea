-- Projects an admin creates in the app. Records already carry a project_id;
-- this registry names the projects a request may use and holds their
-- settings. The built-in projects need no row.
CREATE TABLE projects (
    id              text        PRIMARY KEY CHECK (id ~ '^[a-z0-9][a-z0-9-]{0,63}$'),
    name            text        NOT NULL CHECK (char_length(name) <= 128),
    archived        boolean     NOT NULL DEFAULT false,
    default_runtime text        NOT NULL DEFAULT '' CHECK (char_length(default_runtime) <= 64),
    storage_limit   bigint      NOT NULL DEFAULT 0 CHECK (storage_limit >= 0),
    retention_runs  integer     NOT NULL DEFAULT 0 CHECK (retention_runs >= 0),
    -- Nanoseconds, as Go's time.Duration.
    retention_age   bigint      NOT NULL DEFAULT 0 CHECK (retention_age >= 0),
    created_by      text        NOT NULL CHECK (char_length(created_by) <= 256),
    created_at      timestamptz NOT NULL,
    updated_by      text        NOT NULL DEFAULT '' CHECK (char_length(updated_by) <= 256),
    updated_at      timestamptz
);

-- The projects a library artifact is shared with: IDs, or '*' alone.
CREATE TABLE library_audiences (
    artifact_id text        PRIMARY KEY CHECK (char_length(artifact_id) BETWEEN 1 AND 128),
    projects    text[]      NOT NULL CHECK (cardinality(projects) BETWEEN 1 AND 1000),
    set_by      text        NOT NULL CHECK (char_length(set_by) <= 256),
    set_at      timestamptz NOT NULL
);

-- The projects a runtime is available to: IDs, or '*' alone. A runtime
-- without a row is available to every project.
CREATE TABLE runtime_availability (
    runtime  text        PRIMARY KEY CHECK (char_length(runtime) BETWEEN 1 AND 64),
    projects text[]      NOT NULL CHECK (cardinality(projects) BETWEEN 1 AND 1000),
    set_by   text        NOT NULL CHECK (char_length(set_by) <= 256),
    set_at   timestamptz NOT NULL
);

-- The set's revision: one row that every write to the three tables raises.
CREATE TABLE project_set (
    id       text   PRIMARY KEY DEFAULT 'projects' CHECK (id = 'projects'),
    revision bigint NOT NULL CHECK (revision > 0)
);

-- Every instance follows a change of the set through the change feed.
ALTER TABLE changes DROP CONSTRAINT changes_kind_check;
ALTER TABLE changes ADD CONSTRAINT changes_kind_check
    CHECK (kind IN ('run', 'events', 'evaluation', 'suite', 'deletion', 'job', 'build', 'default_runtime', 'role_bindings', 'projects'));

CREATE TRIGGER project_set_change AFTER INSERT OR UPDATE ON project_set
    FOR EACH ROW EXECUTE FUNCTION record_change('projects');
