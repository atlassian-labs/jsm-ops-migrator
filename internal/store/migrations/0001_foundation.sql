-- Owner: Foundation. Workspace identity and the wrapped data key.
CREATE TABLE meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- One wrapped data key per workspace. provider is the KeyProvider ID that
-- wrapped it; the wrapped value is opaque to the store.
CREATE TABLE data_keys (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    provider   TEXT NOT NULL,
    wrapped    BLOB NOT NULL,
    created_at TEXT NOT NULL
);
