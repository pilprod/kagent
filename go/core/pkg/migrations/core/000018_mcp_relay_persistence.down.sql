DROP TABLE IF EXISTS mcp_relay_grant;

ALTER TABLE runtime_revision
    DROP CONSTRAINT IF EXISTS runtime_revision_mcp_policy_object_check,
    DROP COLUMN IF EXISTS mcp_policy;
