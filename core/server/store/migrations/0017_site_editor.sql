-- The website editor of the admin UI (specs/067-site-editor, core/server/siteedit).

-- A change that has been previewed and not yet published. base_hash identifies the `site:` section it
-- was prepared from: publishing over a section that changed since is refused.
CREATE TABLE site_drafts (
  id         TEXT PRIMARY KEY,
  actor      TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  kind       TEXT NOT NULL,            -- publish | restore
  base_hash  TEXT NOT NULL,
  site_json  TEXT NOT NULL,
  state      TEXT NOT NULL             -- open | published
);

-- What the `site:` section was, newest last: every publish, and what was found there before it (a
-- change made with the CLI). Rolling back publishes one of these again.
CREATE TABLE site_revisions (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  created_at INTEGER NOT NULL,
  actor      TEXT NOT NULL,
  kind       TEXT NOT NULL,            -- publish | restore | external
  hash       TEXT NOT NULL,
  site_json  TEXT NOT NULL
);
