-- The exercise list cursor. It was the implicit rowid, which VACUUM may renumber (the table's key is
-- the text id): a cursor held across a VACUUM could then skip or repeat requests. seq is a stored
-- column, filled from the rowid once so that the order stays, and never changes afterwards. Exercises
-- are never deleted, so the highest value plus one is always new.
ALTER TABLE exercises ADD COLUMN seq INTEGER;
UPDATE exercises SET seq = rowid;
CREATE UNIQUE INDEX exercises_seq ON exercises (seq);
CREATE TRIGGER exercises_assign_seq AFTER INSERT ON exercises
BEGIN UPDATE exercises SET seq = (SELECT COALESCE(MAX(seq), 0) + 1 FROM exercises) WHERE id = NEW.id; END;
