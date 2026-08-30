ALTER TABLE runtime_revision
    DROP CONSTRAINT IF EXISTS runtime_revision_placement_check;

ALTER TABLE runtime_revision
    DROP COLUMN IF EXISTS placement;
