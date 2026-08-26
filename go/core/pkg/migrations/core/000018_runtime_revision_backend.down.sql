ALTER TABLE runtime_revision
    DROP CONSTRAINT IF EXISTS runtime_revision_external_runtime_check,
    DROP CONSTRAINT IF EXISTS runtime_revision_backend_kind_check,
    DROP COLUMN IF EXISTS external_runtime,
    DROP COLUMN IF EXISTS backend_kind;
