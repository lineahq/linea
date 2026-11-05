-- The team's default runtime: one row, set by an admin in the
-- app. Until it is set, a server runs requests that name no runtime on the
-- built-in one.
CREATE TABLE default_runtime (
    id     text        PRIMARY KEY DEFAULT 'default' CHECK (id = 'default'),
    name   text        NOT NULL CHECK (char_length(name) BETWEEN 1 AND 128),
    set_by text        NOT NULL CHECK (char_length(set_by) <= 256),
    set_at timestamptz NOT NULL
);

-- Every instance follows a change of the default through the change feed.
ALTER TABLE changes DROP CONSTRAINT changes_kind_check;
ALTER TABLE changes ADD CONSTRAINT changes_kind_check
    CHECK (kind IN ('run', 'events', 'evaluation', 'suite', 'deletion', 'job', 'build', 'default_runtime'));

CREATE TRIGGER default_runtime_change AFTER INSERT OR UPDATE ON default_runtime
    FOR EACH ROW EXECUTE FUNCTION record_change('default_runtime');
