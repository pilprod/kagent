DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM information_schema.columns
        WHERE table_schema = current_schema()
          AND table_name = 'runtime_revision'
          AND column_name = 'external_profile'
    ) THEN
        EXECUTE $query$
            UPDATE runtime_revision
            SET source_snapshot = jsonb_build_object(
                '__kagent_external_profile_compat_v1',
                jsonb_build_object(
                    'externalRuntime', external_runtime,
                    'externalProfile', external_profile,
                    'sourceSnapshot', source_snapshot
                )
            )
            WHERE backend_kind = 'external'
        $query$;
    END IF;
END
$$;

ALTER TABLE runtime_revision
    DROP CONSTRAINT IF EXISTS runtime_revision_backend_identity_check,
    DROP CONSTRAINT IF EXISTS runtime_revision_external_runtime_check,
    ADD CONSTRAINT runtime_revision_external_runtime_check
        CHECK (
            (backend_kind = 'substrate' AND COALESCE(external_runtime, '') = '')
            OR
            (backend_kind = 'external' AND external_runtime IN ('codex', 'claude'))
        ),
    DROP COLUMN IF EXISTS external_profile;
