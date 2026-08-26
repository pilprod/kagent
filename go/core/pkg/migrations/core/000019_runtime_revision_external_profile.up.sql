DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM runtime_revision
        WHERE (
              backend_kind = 'external'
              OR (
                  actor_template_namespace = '_external'
                  AND actor_template_name = revision
                  AND actor_template_uid = ''
              )
          )
          AND NOT COALESCE((
              jsonb_typeof(source_snapshot -> '__kagent_external_profile_compat_v1') = 'object'
              AND (source_snapshot #>> '{__kagent_external_profile_compat_v1,externalRuntime}') IN ('codex', 'claude')
              AND jsonb_typeof(source_snapshot #> '{__kagent_external_profile_compat_v1,externalProfile}') = 'object'
              AND source_snapshot #> '{__kagent_external_profile_compat_v1,sourceSnapshot}' IS NOT NULL
          ), FALSE)
    ) THEN
        RAISE EXCEPTION 'cannot add external runtime profiles: version 18 contains external revisions without recoverable profiles';
    END IF;
END
$$;

ALTER TABLE runtime_revision
    ADD COLUMN IF NOT EXISTS external_profile JSONB;

UPDATE runtime_revision
SET backend_kind = 'external',
    external_runtime = source_snapshot #>> '{__kagent_external_profile_compat_v1,externalRuntime}',
    external_profile = source_snapshot #> '{__kagent_external_profile_compat_v1,externalProfile}',
    source_snapshot = source_snapshot #> '{__kagent_external_profile_compat_v1,sourceSnapshot}'
WHERE actor_template_namespace = '_external'
  AND actor_template_name = revision
  AND actor_template_uid = ''
  AND jsonb_typeof(source_snapshot -> '__kagent_external_profile_compat_v1') = 'object'
  AND (source_snapshot #>> '{__kagent_external_profile_compat_v1,externalRuntime}') IN ('codex', 'claude')
  AND jsonb_typeof(source_snapshot #> '{__kagent_external_profile_compat_v1,externalProfile}') = 'object'
  AND source_snapshot #> '{__kagent_external_profile_compat_v1,sourceSnapshot}' IS NOT NULL;

UPDATE runtime_revision
SET external_runtime = NULL
WHERE backend_kind = 'substrate'
  AND external_runtime = '';

ALTER TABLE runtime_revision
    DROP CONSTRAINT IF EXISTS runtime_revision_external_runtime_check,
    DROP CONSTRAINT IF EXISTS runtime_revision_backend_identity_check,
    ADD CONSTRAINT runtime_revision_backend_identity_check
        CHECK (
            (
                backend_kind = 'substrate'
                AND external_runtime IS NULL
                AND external_profile IS NULL
            )
            OR
            (
                backend_kind = 'external'
                AND external_runtime IS NOT NULL
                AND external_runtime IN ('codex', 'claude')
                AND external_profile IS NOT NULL
                AND jsonb_typeof(external_profile) = 'object'
                AND actor_template_namespace = '_external'
                AND actor_template_name = revision
                AND actor_template_uid = ''
                AND phase = 'Ready'
                AND golden_snapshot = ''
            )
        );
