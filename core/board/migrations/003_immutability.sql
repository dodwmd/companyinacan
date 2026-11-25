-- Append-only and immutability guarantees enforced by the database, not only by the service. A database
-- administrator can still drop these triggers or rewrite the file; the audit hash chain (and a copy of its
-- latest hash kept elsewhere) is what makes that visible (docs/board.md).

CREATE TRIGGER audit_log_no_update BEFORE UPDATE ON audit_log
BEGIN SELECT RAISE(ABORT, 'audit_log is append-only'); END;
CREATE TRIGGER audit_log_no_delete BEFORE DELETE ON audit_log
BEGIN SELECT RAISE(ABORT, 'audit_log is append-only'); END;

-- Bodies and meetings are never deleted.
CREATE TRIGGER bodies_no_delete BEFORE DELETE ON bodies
BEGIN SELECT RAISE(ABORT, 'bodies are never deleted'); END;
CREATE TRIGGER meetings_no_delete BEFORE DELETE ON meetings
BEGIN SELECT RAISE(ABORT, 'meetings are never deleted: cancel them'); END;
CREATE TRIGGER meetings_frozen BEFORE UPDATE ON meetings
WHEN NEW.id != OLD.id OR NEW.body_id != OLD.body_id OR NEW.created_by != OLD.created_by
BEGIN SELECT RAISE(ABORT, 'a meeting cannot move to another body'); END;

-- Terms: only the end of a term may be written.
CREATE TRIGGER members_no_delete BEFORE DELETE ON body_members
BEGIN SELECT RAISE(ABORT, 'membership history is kept: end the term'); END;
CREATE TRIGGER members_frozen BEFORE UPDATE ON body_members
WHEN NEW.id != OLD.id OR NEW.body_id != OLD.body_id OR NEW.person_id != OLD.person_id OR NEW.role != OLD.role
  OR NEW.start_date != OLD.start_date OR OLD.end_date IS NOT NULL
BEGIN SELECT RAISE(ABORT, 'a term can only be ended once'); END;

-- Resolutions move forward only and are frozen once decided.
CREATE TRIGGER resolutions_no_delete BEFORE DELETE ON resolutions
BEGIN SELECT RAISE(ABORT, 'resolutions are never deleted: withdraw them'); END;
CREATE TRIGGER resolutions_decided_frozen BEFORE UPDATE ON resolutions
WHEN OLD.status IN ('passed', 'failed', 'withdrawn')
BEGIN SELECT RAISE(ABORT, 'a decided resolution is immutable: the tally is final'); END;
CREATE TRIGGER resolutions_forward BEFORE UPDATE ON resolutions
WHEN NEW.status != OLD.status AND NOT (
     (OLD.status = 'draft' AND NEW.status IN ('proposed', 'withdrawn'))
  OR (OLD.status = 'proposed' AND NEW.status IN ('voting', 'withdrawn'))
  OR (OLD.status = 'voting' AND NEW.status IN ('passed', 'failed', 'withdrawn')))
BEGIN SELECT RAISE(ABORT, 'invalid resolution status change'); END;
CREATE TRIGGER resolutions_identity BEFORE UPDATE ON resolutions
WHEN NEW.id != OLD.id OR NEW.body_id != OLD.body_id OR NEW.kind != OLD.kind OR NEW.proposer_ref != OLD.proposer_ref
BEGIN SELECT RAISE(ABORT, 'resolution identity is fixed'); END;
-- Once voting opens the thresholds and the text version are fixed.
CREATE TRIGGER resolutions_terms_fixed BEFORE UPDATE ON resolutions
WHEN OLD.status = 'voting' AND (NEW.pass_num != OLD.pass_num OR NEW.pass_den != OLD.pass_den OR NEW.pass_inclusive != OLD.pass_inclusive
  OR NEW.current_version != OLD.current_version OR NEW.secret_ballot != OLD.secret_ballot OR NEW.eligible_count != OLD.eligible_count)
BEGIN SELECT RAISE(ABORT, 'the terms are fixed once voting has opened'); END;

-- Versions: appended while the resolution can still be amended; only the sealed columns may be rewritten (key rotation).
CREATE TRIGGER versions_no_delete BEFORE DELETE ON resolution_versions
BEGIN SELECT RAISE(ABORT, 'resolution versions are kept'); END;
CREATE TRIGGER versions_frozen BEFORE UPDATE ON resolution_versions
WHEN NEW.resolution_id != OLD.resolution_id OR NEW.version != OLD.version OR NEW.content_sha256 != OLD.content_sha256
  OR NEW.created_by != OLD.created_by OR NEW.created_at != OLD.created_at
BEGIN SELECT RAISE(ABORT, 'resolution versions are append-only: amend to create a new version'); END;
CREATE TRIGGER versions_only_before_voting BEFORE INSERT ON resolution_versions
WHEN (SELECT status FROM resolutions WHERE id = NEW.resolution_id) NOT IN ('draft', 'proposed')
BEGIN SELECT RAISE(ABORT, 'a resolution can only be amended before voting opens'); END;

-- Votes, signatures and ballots exist only while voting is open, and never change.
CREATE TRIGGER votes_only_while_voting BEFORE INSERT ON votes
WHEN (SELECT status FROM resolutions WHERE id = NEW.resolution_id) != 'voting'
BEGIN SELECT RAISE(ABORT, 'votes can only be cast while voting is open'); END;
CREATE TRIGGER votes_no_update BEFORE UPDATE ON votes
BEGIN SELECT RAISE(ABORT, 'votes are append-only'); END;
CREATE TRIGGER votes_no_delete BEFORE DELETE ON votes
BEGIN SELECT RAISE(ABORT, 'votes are append-only'); END;
CREATE TRIGGER ballots_only_while_voting BEFORE INSERT ON ballots
WHEN (SELECT status FROM resolutions WHERE id = NEW.resolution_id) != 'voting'
BEGIN SELECT RAISE(ABORT, 'ballots can only be cast while voting is open'); END;
CREATE TRIGGER ballots_frozen BEFORE UPDATE ON ballots
WHEN NEW.id != OLD.id OR NEW.resolution_id != OLD.resolution_id
BEGIN SELECT RAISE(ABORT, 'ballots are append-only'); END;
CREATE TRIGGER ballots_no_delete BEFORE DELETE ON ballots
BEGIN SELECT RAISE(ABORT, 'ballots are append-only'); END;
CREATE TRIGGER receipts_only_while_voting BEFORE INSERT ON vote_receipts
WHEN (SELECT status FROM resolutions WHERE id = NEW.resolution_id) != 'voting'
BEGIN SELECT RAISE(ABORT, 'votes can only be cast while voting is open'); END;
CREATE TRIGGER receipts_no_update BEFORE UPDATE ON vote_receipts
BEGIN SELECT RAISE(ABORT, 'vote receipts are append-only'); END;
CREATE TRIGGER receipts_no_delete BEFORE DELETE ON vote_receipts
BEGIN SELECT RAISE(ABORT, 'vote receipts are append-only'); END;

-- The voter list is fixed when voting opens (the resolution moves proposed -> voting in the same transaction).
CREATE TRIGGER voters_only_when_opening BEFORE INSERT ON resolution_voters
WHEN (SELECT status FROM resolutions WHERE id = NEW.resolution_id) != 'proposed'
BEGIN SELECT RAISE(ABORT, 'the voter list is fixed when voting opens'); END;
CREATE TRIGGER voters_no_update BEFORE UPDATE ON resolution_voters
BEGIN SELECT RAISE(ABORT, 'the voter list is fixed'); END;
CREATE TRIGGER voters_no_delete BEFORE DELETE ON resolution_voters
BEGIN SELECT RAISE(ABORT, 'the voter list is fixed'); END;

-- Minutes: an approved version never changes (its content hash is frozen; only the sealed ciphertext may be
-- rewritten by key rotation) and may only become superseded by a later approved version.
CREATE TRIGGER minutes_no_delete BEFORE DELETE ON minutes
BEGIN SELECT RAISE(ABORT, 'minutes are statutory records and are never deleted'); END;
CREATE TRIGGER minutes_identity BEFORE UPDATE ON minutes
WHEN NEW.id != OLD.id OR NEW.meeting_id != OLD.meeting_id OR NEW.version != OLD.version
BEGIN SELECT RAISE(ABORT, 'minutes identity is fixed'); END;
CREATE TRIGGER minutes_approved_frozen BEFORE UPDATE ON minutes
WHEN OLD.status IN ('approved', 'superseded') AND (NEW.content_sha256 != OLD.content_sha256
  OR NEW.author_ref != OLD.author_ref OR NEW.approved_by != OLD.approved_by OR IFNULL(NEW.approved_on, '') != IFNULL(OLD.approved_on, '')
  OR NEW.edited_by != OLD.edited_by OR NEW.status NOT IN (OLD.status, 'superseded') OR (OLD.status = 'superseded' AND NEW.status != 'superseded'))
BEGIN SELECT RAISE(ABORT, 'approved minutes are immutable: draft a new version'); END;

-- Attendance is frozen once the meeting has approved minutes.
CREATE TRIGGER attendance_frozen_ins BEFORE INSERT ON attendance
WHEN EXISTS (SELECT 1 FROM minutes WHERE meeting_id = NEW.meeting_id AND status IN ('approved', 'superseded'))
BEGIN SELECT RAISE(ABORT, 'attendance is part of approved minutes and cannot change'); END;
CREATE TRIGGER attendance_frozen_upd BEFORE UPDATE ON attendance
WHEN EXISTS (SELECT 1 FROM minutes WHERE meeting_id = OLD.meeting_id AND status IN ('approved', 'superseded'))
BEGIN SELECT RAISE(ABORT, 'attendance is part of approved minutes and cannot change'); END;
CREATE TRIGGER attendance_no_delete BEFORE DELETE ON attendance
BEGIN SELECT RAISE(ABORT, 'attendance is never deleted: record the change'); END;

CREATE TRIGGER pack_no_delete BEFORE DELETE ON pack_items
BEGIN SELECT RAISE(ABORT, 'pack items are kept'); END;

-- Registers: append-only ledgers. Only the attestation (once) and the sealed detail (key rotation, erasure of
-- personal details that are not statutory) may change.
CREATE TRIGGER register_no_delete BEFORE DELETE ON register_entries
BEGIN SELECT RAISE(ABORT, 'statutory registers are append-only: record a correcting entry'); END;
CREATE TRIGGER register_frozen BEFORE UPDATE ON register_entries
WHEN NEW.id != OLD.id OR NEW.seq != OLD.seq OR NEW.entity_id != OLD.entity_id OR NEW.register != OLD.register OR NEW.kind != OLD.kind
  OR NEW.party_ref != OLD.party_ref OR NEW.office != OLD.office OR NEW.title != OLD.title OR NEW.class_ref != OLD.class_ref OR NEW.quantity != OLD.quantity
  OR NEW.effective_on != OLD.effective_on OR NEW.resolution_ref != OLD.resolution_ref OR IFNULL(NEW.corrects_id, '') != IFNULL(OLD.corrects_id, '')
  OR NEW.recorded_by != OLD.recorded_by OR NEW.recorded_at != OLD.recorded_at
  OR (OLD.attested_on IS NOT NULL AND (NEW.attested_by != OLD.attested_by OR IFNULL(NEW.attested_on, '') != IFNULL(OLD.attested_on, '')))
BEGIN SELECT RAISE(ABORT, 'statutory registers are append-only: record a correcting entry'); END;

-- Contacts: history is kept; only the end date may be set, once.
CREATE TRIGGER contacts_no_delete BEFORE DELETE ON entity_contacts
BEGIN SELECT RAISE(ABORT, 'contact history is kept'); END;
CREATE TRIGGER contacts_frozen BEFORE UPDATE ON entity_contacts
WHEN NEW.id != OLD.id OR NEW.entity_id != OLD.entity_id OR NEW.kind != OLD.kind OR NEW.effective_from != OLD.effective_from OR NEW.person_ref != OLD.person_ref
  OR NEW.created_by != OLD.created_by OR (OLD.effective_to IS NOT NULL AND IFNULL(NEW.effective_to, '') != OLD.effective_to)
BEGIN SELECT RAISE(ABORT, 'contact history is append-only'); END;

CREATE TRIGGER legal_entities_no_delete BEFORE DELETE ON legal_entities
BEGIN SELECT RAISE(ABORT, 'legal entities are never deleted: mark them dissolved'); END;
CREATE TRIGGER obligations_no_delete BEFORE DELETE ON filing_obligations
BEGIN SELECT RAISE(ABORT, 'obligations are never deleted: deactivate them'); END;
CREATE TRIGGER filings_no_delete BEFORE DELETE ON filings
BEGIN SELECT RAISE(ABORT, 'filings are never deleted'); END;
CREATE TRIGGER filings_done_frozen BEFORE UPDATE ON filings
WHEN OLD.status IN ('filed', 'waived') AND (NEW.status != OLD.status OR NEW.due_on != OLD.due_on OR NEW.filed_on IS NOT OLD.filed_on OR NEW.filed_by != OLD.filed_by OR NEW.doc_ref != OLD.doc_ref)
BEGIN SELECT RAISE(ABORT, 'a recorded filing does not change'); END;
CREATE TRIGGER entity_documents_no_delete BEFORE DELETE ON entity_documents
BEGIN SELECT RAISE(ABORT, 'document references are kept'); END;
CREATE TRIGGER share_class_links_no_delete BEFORE DELETE ON share_class_links
BEGIN SELECT RAISE(ABORT, 'share class links are kept'); END;
