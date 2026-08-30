-- The default is deliberately retained after backfill. During a rolling
-- upgrade, a previous controller binary does not write mcp_policy; its new
-- revisions therefore receive an explicit deny-all policy instead of NULL or
-- an accidentally permissive value.
ALTER TABLE runtime_revision
    ADD COLUMN IF NOT EXISTS mcp_policy JSONB NOT NULL
        DEFAULT '{"version":"v1","bindings":[]}'::jsonb;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'runtime_revision_mcp_policy_object_check'
          AND conrelid = 'runtime_revision'::regclass
    ) THEN
        ALTER TABLE runtime_revision
            ADD CONSTRAINT runtime_revision_mcp_policy_object_check
                CHECK (jsonb_typeof(mcp_policy) = 'object');
    END IF;
END
$$;

-- Only capability digests cross the persistence boundary. There is
-- intentionally no plaintext token or connection-material column.
CREATE TABLE IF NOT EXISTS mcp_relay_grant (
    capability_hash BYTEA PRIMARY KEY,
    agent_instance_id UUID NOT NULL REFERENCES agent_instance(id) ON DELETE CASCADE,
    revision TEXT NOT NULL REFERENCES runtime_revision(revision) ON DELETE CASCADE,
    binding_id TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT mcp_relay_grant_capability_hash_size_check
        CHECK (octet_length(capability_hash) = 32),
    CONSTRAINT mcp_relay_grant_capability_hash_nonzero_check
        CHECK (capability_hash <> decode(repeat('00', 32), 'hex')),
    CONSTRAINT mcp_relay_grant_binding_id_check
        CHECK (binding_id ~ '^mcp-[0-9a-f]{64}$'),
    CONSTRAINT mcp_relay_grant_expiry_check
        CHECK (
            expires_at > '1970-01-01 00:00:00+00'::timestamptz
            AND isfinite(expires_at)
        )
);

CREATE INDEX IF NOT EXISTS mcp_relay_grant_agent_instance_idx
    ON mcp_relay_grant (agent_instance_id);

CREATE INDEX IF NOT EXISTS mcp_relay_grant_revision_idx
    ON mcp_relay_grant (revision);
