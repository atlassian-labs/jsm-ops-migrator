-- Owner: Run engine. A reviewed mapping set belongs to one immutable plan.
CREATE TABLE mapping_approvals (
    plan_version INTEGER PRIMARY KEY REFERENCES plan_versions(version),
    record BLOB NOT NULL
);
