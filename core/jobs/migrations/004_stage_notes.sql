-- A stage move made from the web UI carries a short reason. It is candidate-related free text, so
-- it is sealed like the rejection note (the key is derived from table, row id and column) and is
-- purged with the other evaluations when a candidate is erased or ages out. Moves through the
-- JSON API without a note leave it NULL.
ALTER TABLE stage_history ADD COLUMN note_sealed TEXT;
