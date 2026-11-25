-- Append-only and immutability guarantees enforced by the database, not only by the service.
-- A database administrator can still drop these triggers or rewrite the file; the audit hash
-- chain (and a copy of its latest hash kept elsewhere) is what makes that visible (docs/equity.md).

CREATE TRIGGER audit_log_no_update BEFORE UPDATE ON audit_log
BEGIN SELECT RAISE(ABORT, 'audit_log is append-only'); END;
CREATE TRIGGER audit_log_no_delete BEFORE DELETE ON audit_log
BEGIN SELECT RAISE(ABORT, 'audit_log is append-only'); END;

CREATE TRIGGER ledger_no_update BEFORE UPDATE ON ledger
BEGIN SELECT RAISE(ABORT, 'the equity ledger is append-only: record a correcting entry'); END;
CREATE TRIGGER ledger_no_delete BEFORE DELETE ON ledger
BEGIN SELECT RAISE(ABORT, 'the equity ledger is append-only: record a correcting entry'); END;

CREATE TRIGGER milestones_no_update BEFORE UPDATE ON grant_milestones
BEGIN SELECT RAISE(ABORT, 'grant_milestones is append-only'); END;
CREATE TRIGGER milestones_no_delete BEFORE DELETE ON grant_milestones
BEGIN SELECT RAISE(ABORT, 'grant_milestones is append-only'); END;
CREATE TRIGGER accel_no_update BEFORE UPDATE ON grant_accel_events
BEGIN SELECT RAISE(ABORT, 'grant_accel_events is append-only'); END;
CREATE TRIGGER accel_no_delete BEFORE DELETE ON grant_accel_events
BEGIN SELECT RAISE(ABORT, 'grant_accel_events is append-only'); END;

-- Valuations: only the sealed columns may change (key rotation, retention clearing).
CREATE TRIGGER valuations_no_delete BEFORE DELETE ON valuations
BEGIN SELECT RAISE(ABORT, 'valuations are append-only'); END;
CREATE TRIGGER valuations_frozen BEFORE UPDATE ON valuations
WHEN NEW.id != OLD.id OR NEW.entity_id != OLD.entity_id OR IFNULL(NEW.class_id, '') != IFNULL(OLD.class_id, '') OR NEW.kind != OLD.kind
  OR NEW.effective_date != OLD.effective_date OR NEW.expires_on != OLD.expires_on OR NEW.created_by != OLD.created_by OR NEW.created_at != OLD.created_at
BEGIN SELECT RAISE(ABORT, 'valuations are append-only'); END;

CREATE TRIGGER convertibles_no_delete BEFORE DELETE ON convertibles
BEGIN SELECT RAISE(ABORT, 'convertibles are never deleted: cancel them'); END;
CREATE TRIGGER convertibles_frozen BEFORE UPDATE ON convertibles
WHEN NEW.id != OLD.id OR NEW.entity_id != OLD.entity_id OR NEW.stakeholder_id != OLD.stakeholder_id OR NEW.kind != OLD.kind OR NEW.issued_on != OLD.issued_on
BEGIN SELECT RAISE(ABORT, 'convertible terms are frozen'); END;

CREATE TRIGGER stakeholders_no_delete BEFORE DELETE ON stakeholders
BEGIN SELECT RAISE(ABORT, 'stakeholders are pseudonymised, never deleted'); END;
CREATE TRIGGER stakeholders_frozen BEFORE UPDATE ON stakeholders
WHEN NEW.id != OLD.id OR NEW.entity_id != OLD.entity_id OR NEW.kind != OLD.kind OR NEW.created_by != OLD.created_by
BEGIN SELECT RAISE(ABORT, 'stakeholder kind is fixed'); END;

-- Authorised shares may be raised, never lowered; nothing else about a class changes.
CREATE TRIGGER share_classes_frozen BEFORE UPDATE ON share_classes
WHEN NEW.id != OLD.id OR NEW.entity_id != OLD.entity_id OR NEW.name != OLD.name OR NEW.kind != OLD.kind
  OR NEW.par_minor != OLD.par_minor OR NEW.currency != OLD.currency OR NEW.authorised < OLD.authorised
BEGIN SELECT RAISE(ABORT, 'a share class can only have its authorised shares raised'); END;
CREATE TRIGGER share_classes_no_delete BEFORE DELETE ON share_classes
BEGIN SELECT RAISE(ABORT, 'share classes are never deleted'); END;

-- Grants: terms are frozen once the grant leaves draft; status moves only along the lifecycle;
-- a grant can only be approved by someone who did not create, edit or submit it; only drafts
-- are deleted; what was exercised never goes down.
CREATE TRIGGER grants_terms_frozen BEFORE UPDATE ON grants
WHEN OLD.status != 'draft' AND (
     NEW.id != OLD.id OR NEW.entity_id != OLD.entity_id OR NEW.plan_id != OLD.plan_id OR NEW.stakeholder_id != OLD.stakeholder_id
  OR NEW.type != OLD.type OR NEW.quantity != OLD.quantity OR NEW.currency != OLD.currency OR NEW.strike_minor != OLD.strike_minor
  OR NEW.grant_date != OLD.grant_date OR NEW.vest_start != OLD.vest_start OR NEW.schedule != OLD.schedule
  OR NEW.expiry_date != OLD.expiry_date OR NEW.ptep_days != OLD.ptep_days OR NEW.version != OLD.version
  OR IFNULL(NEW.supersedes_id, '') != IFNULL(OLD.supersedes_id, '') OR NEW.created_by != OLD.created_by)
BEGIN SELECT RAISE(ABORT, 'grant terms are frozen after approval: amend creates a new version'); END;

CREATE TRIGGER grants_lifecycle BEFORE UPDATE ON grants
WHEN NEW.status != OLD.status AND NOT ((OLD.status, NEW.status) IN (VALUES
     ('draft', 'pending_approval'), ('draft', 'cancelled'),
     ('pending_approval', 'draft'), ('pending_approval', 'approved'), ('pending_approval', 'cancelled'),
     ('approved', 'active'), ('approved', 'cancelled'),
     ('active', 'cancelled'), ('active', 'forfeited'), ('active', 'expired')))
BEGIN SELECT RAISE(ABORT, 'illegal grant status transition'); END;

CREATE TRIGGER grants_approval_sod BEFORE UPDATE ON grants
WHEN NEW.status = 'approved' AND OLD.status != 'approved' AND (
     NEW.approved_by IS NULL OR NEW.approved_by = OLD.created_by OR NEW.approved_by = IFNULL(OLD.submitted_by, '')
  OR EXISTS (SELECT 1 FROM json_each(OLD.edited_by) WHERE value = NEW.approved_by))
BEGIN SELECT RAISE(ABORT, 'separation of duties: the approver must not have created, edited or submitted the grant'); END;

CREATE TRIGGER grants_exercised_monotone BEFORE UPDATE ON grants
WHEN NEW.exercised < OLD.exercised OR NEW.exercised > NEW.quantity
BEGIN SELECT RAISE(ABORT, 'exercised quantity cannot decrease or exceed the grant'); END;

CREATE TRIGGER grants_delete_draft_only BEFORE DELETE ON grants
WHEN OLD.status != 'draft'
BEGIN SELECT RAISE(ABORT, 'only draft grants can be deleted'); END;

-- Exercises: the request is frozen; the chain needs two different people.
CREATE TRIGGER exercises_no_delete BEFORE DELETE ON exercises
BEGIN SELECT RAISE(ABORT, 'exercises are never deleted'); END;
CREATE TRIGGER exercises_frozen BEFORE UPDATE ON exercises
WHEN NEW.id != OLD.id OR NEW.grant_id != OLD.grant_id OR NEW.stakeholder_id != OLD.stakeholder_id OR NEW.quantity != OLD.quantity
  OR NEW.due_minor != OLD.due_minor OR NEW.requested_by != OLD.requested_by OR NEW.currency != OLD.currency
  OR (OLD.status IN ('issued', 'rejected', 'cancelled') AND NEW.status != OLD.status)
BEGIN SELECT RAISE(ABORT, 'exercise request is frozen'); END;
CREATE TRIGGER exercises_lifecycle BEFORE UPDATE ON exercises
WHEN NEW.status != OLD.status AND NOT ((OLD.status, NEW.status) IN (VALUES
     ('requested', 'approved'), ('requested', 'rejected'), ('requested', 'cancelled'),
     ('approved', 'payment_recorded'), ('approved', 'cancelled'),
     ('payment_recorded', 'issued')))
BEGIN SELECT RAISE(ABORT, 'illegal exercise status transition'); END;
CREATE TRIGGER exercises_sod BEFORE UPDATE ON exercises
WHEN (NEW.status = 'approved' AND OLD.status != 'approved' AND (NEW.approved_by IS NULL OR NEW.approved_by = OLD.requested_by))
  OR (NEW.status = 'payment_recorded' AND OLD.status != 'payment_recorded' AND (NEW.payment_by IS NULL OR NEW.payment_by = OLD.requested_by))
BEGIN SELECT RAISE(ABORT, 'separation of duties: the requester cannot approve or pay their own exercise'); END;
