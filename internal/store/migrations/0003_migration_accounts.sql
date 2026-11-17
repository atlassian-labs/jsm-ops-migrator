-- Owner: Run engine. Older reads lack a verified account and cannot be reused.
ALTER TABLE snapshot_reads ADD COLUMN account BLOB;
CREATE INDEX snapshot_account_by_side ON snapshot_reads(side, id) WHERE account IS NOT NULL;
