-- name: GetRuntimeRevisionMCPPolicy :one
SELECT mcp_policy
FROM runtime_revision
WHERE revision = $1;

-- name: LockRuntimeRevisionMCPPolicy :one
SELECT mcp_policy
FROM runtime_revision
WHERE revision = $1
FOR SHARE;

-- name: InsertMCPRelayGrant :one
INSERT INTO mcp_relay_grant (
    capability_hash, agent_instance_id, revision, binding_id, expires_at, revoked_at
)
SELECT
    sqlc.arg(capability_hash), i.id, r.revision, sqlc.arg(binding_id),
    sqlc.arg(expires_at), sqlc.narg(revoked_at)
FROM agent_instance i
JOIN runtime_revision r ON r.revision = sqlc.arg(revision)
WHERE i.id = sqlc.arg(agent_instance_id)
  AND i.prepared_revision = r.revision
  AND EXISTS (
      SELECT 1
      FROM jsonb_array_elements(r.mcp_policy -> 'bindings') AS binding
      WHERE binding ->> 'id' = sqlc.arg(binding_id)
  )
RETURNING mcp_relay_grant.*;

-- name: GetMCPRelayGrant :one
SELECT *
FROM mcp_relay_grant
WHERE capability_hash = $1;

-- name: RevokeMCPRelayGrant :one
UPDATE mcp_relay_grant
SET revoked_at = COALESCE(revoked_at, sqlc.arg(revoked_at))
WHERE capability_hash = sqlc.arg(capability_hash)
RETURNING *;

-- name: GetMCPRelayInstanceLifecycle :one
SELECT id, prepared_revision, state, operation
FROM agent_instance
WHERE id = $1;
