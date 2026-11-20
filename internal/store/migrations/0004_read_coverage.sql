-- Owner: Run engine. The steps that made each read, with the format each
-- wrote its part in, so a read is reused only by the same steps. Older reads
-- have none and are never reused. It is kept in clear, like the other IDs:
-- it names this build's steps, not anything a step read.
ALTER TABLE snapshot_reads ADD COLUMN readers TEXT;
