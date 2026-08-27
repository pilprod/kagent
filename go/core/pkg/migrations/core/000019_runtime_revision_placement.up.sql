-- The default is deliberately retained for rolling-upgrade compatibility.
-- Revisions written by an older controller could only use the Kubernetes Pod
-- worker provider, so defaulting them cannot silently opt into an external
-- execution boundary.
ALTER TABLE runtime_revision
    ADD COLUMN IF NOT EXISTS placement TEXT NOT NULL DEFAULT 'KubernetesPod';

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'runtime_revision_placement_check'
          AND conrelid = 'runtime_revision'::regclass
    ) THEN
        ALTER TABLE runtime_revision
            ADD CONSTRAINT runtime_revision_placement_check
                CHECK (placement IN ('KubernetesPod', 'ExternalSlot'));
    END IF;
END
$$;
