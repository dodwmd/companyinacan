-- specs/065-work-manager: who is on a project, saved filters, and when an issue got the priority it
-- has now (for the P0/P1 ageing view).

-- A roster, not an access list: it does not limit who can see a project.
CREATE TABLE project_members (
    project_id INTEGER NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    subject    TEXT    NOT NULL,
    added_by   TEXT    NOT NULL,
    added_at   INTEGER NOT NULL,
    PRIMARY KEY (project_id, subject)
);

CREATE TABLE saved_filters (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    owner      TEXT    NOT NULL,
    name       TEXT    NOT NULL,
    project    TEXT    NOT NULL DEFAULT '',  -- a project key, or empty for every project
    query      TEXT    NOT NULL DEFAULT '',  -- the search language (query.go)
    sort       TEXT    NOT NULL DEFAULT '',
    shared     INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    UNIQUE (owner, name)
);

ALTER TABLE issues ADD COLUMN priority_since INTEGER NOT NULL DEFAULT 0;
UPDATE issues SET priority_since = COALESCE(
    (SELECT MAX(a.at) FROM activity a WHERE a.issue_id = issues.id AND a.kind = 'updated' AND json_extract(a.data, '$.priority') IS NOT NULL),
    created_at);
