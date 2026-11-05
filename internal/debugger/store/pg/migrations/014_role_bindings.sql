-- Role bindings an admin manages in the app, beside the ones the team file
-- declares. A binding grants a role in a scope to one group or principal;
-- the server validates the role and scope.
CREATE TABLE role_bindings (
    id           text        PRIMARY KEY CHECK (id ~ '^[0-9a-f]{32}$'),
    subject_kind text        NOT NULL CHECK (subject_kind IN ('group', 'principal')),
    subject      text        NOT NULL CHECK (char_length(subject) BETWEEN 1 AND 256),
    role         text        NOT NULL CHECK (char_length(role) BETWEEN 1 AND 64),
    scope        text        NOT NULL CHECK (char_length(scope) BETWEEN 1 AND 256),
    created_by   text        NOT NULL CHECK (char_length(created_by) <= 256),
    created_at   timestamptz NOT NULL,
    UNIQUE (subject_kind, subject, role, scope)
);

-- The set's revision: one row that every write raises, so an edit made from
-- a stale list conflicts.
CREATE TABLE role_binding_set (
    id       text   PRIMARY KEY DEFAULT 'bindings' CHECK (id = 'bindings'),
    revision bigint NOT NULL CHECK (revision > 0)
);

-- Every instance follows a change of the bindings through the change feed.
ALTER TABLE changes DROP CONSTRAINT changes_kind_check;
ALTER TABLE changes ADD CONSTRAINT changes_kind_check
    CHECK (kind IN ('run', 'events', 'evaluation', 'suite', 'deletion', 'job', 'build', 'default_runtime', 'role_bindings'));

CREATE TRIGGER role_binding_set_change AFTER INSERT OR UPDATE ON role_binding_set
    FOR EACH ROW EXECUTE FUNCTION record_change('role_bindings');
