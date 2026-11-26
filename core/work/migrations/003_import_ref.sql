-- specs/047-csv-import: external_id is the issue's id in the system it was imported from, so a
-- re-run of an import never creates a second issue. Unique within a project.
ALTER TABLE issues ADD COLUMN external_id TEXT;
CREATE UNIQUE INDEX issues_external_id ON issues (project_id, external_id) WHERE external_id IS NOT NULL;
