-- The retention purge deletes invoices: for each one SQLite looks for rows that refer to it (the foreign keys
-- below) and the purge asks whether a credit note points at it. Without these indexes each of those is a table
-- scan, so the purge grows with the square of the invoices while it holds the only writer connection.
CREATE INDEX invoices_original    ON invoices (original_id);
CREATE INDEX time_entries_invoice ON time_entries (invoice_id);
