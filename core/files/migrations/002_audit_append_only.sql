-- The audit log is append-only at the database level: no UPDATE or DELETE through the service
-- connection. A database administrator can still drop these triggers or rewrite the file; the
-- hash chain makes edits to old entries detectable only when the latest hash is also kept
-- somewhere the editor cannot reach (docs/files.md). Audit rows hold opaque ids and field names,
-- never file names or content, so they are not subject to erasure.
CREATE TRIGGER audit_log_no_update BEFORE UPDATE ON audit_log
BEGIN
    SELECT RAISE(ABORT, 'audit_log is append-only');
END;

CREATE TRIGGER audit_log_no_delete BEFORE DELETE ON audit_log
BEGIN
    SELECT RAISE(ABORT, 'audit_log is append-only');
END;
