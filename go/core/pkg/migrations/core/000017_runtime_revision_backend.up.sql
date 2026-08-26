ALTER TABLE runtime_revision
    ADD COLUMN backend_kind TEXT,
    ADD COLUMN external_runtime TEXT;

UPDATE runtime_revision
SET backend_kind = 'substrate'
WHERE backend_kind IS NULL;

ALTER TABLE runtime_revision
    ALTER COLUMN backend_kind SET NOT NULL,
    ADD CONSTRAINT runtime_revision_backend_kind_check
        CHECK (backend_kind IN ('substrate', 'external')),
    ADD CONSTRAINT runtime_revision_external_runtime_check
        CHECK (
            (backend_kind = 'substrate' AND COALESCE(external_runtime, '') = '')
            OR
            (backend_kind = 'external' AND external_runtime IN ('codex', 'claude'))
        );
