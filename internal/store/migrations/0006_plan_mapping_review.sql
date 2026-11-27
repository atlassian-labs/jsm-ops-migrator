-- The menu reads this flag without unlocking the sealed plan. NULL identifies
-- plans produced before review decisions were explicitly entered by the admin.
ALTER TABLE plan_versions ADD COLUMN requires_mapping_review BOOLEAN;
