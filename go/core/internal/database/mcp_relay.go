package database

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	dbgen "github.com/kagent-dev/kagent/go/core/internal/database/gen"
	"github.com/kagent-dev/kagent/go/core/v2/mcprelay"
	"github.com/kagent-dev/kagent/go/core/v2/translator"
)

var (
	// ErrMCPRelayGrantDigest reports the all-zero sentinel, which is never a
	// valid persisted capability digest even though its fixed size is correct.
	ErrMCPRelayGrantDigest = errors.New("MCP relay capability digest is invalid")
	// ErrMCPRelayGrantConflict reports reuse of an already persisted capability
	// digest. The existing grant is never overwritten, even when the requested
	// scope happens to match.
	ErrMCPRelayGrantConflict = errors.New("MCP relay capability digest already exists")
	// ErrMCPRelayGrantScope reports a grant whose instance, prepared revision,
	// or binding does not form one currently persisted authorization scope.
	ErrMCPRelayGrantScope = errors.New("MCP relay grant scope is invalid")
)

const emptyMCPPolicyJSON = `{"version":"v1","bindings":[]}`

var (
	_ mcprelay.PolicyStore    = (*MCPRelayStore)(nil)
	_ mcprelay.GrantVerifier  = (*MCPRelayStore)(nil)
	_ mcprelay.LifecycleStore = (*MCPRelayStore)(nil)
)

// MCPRelayStore is the PostgreSQL-backed, transport-independent persistence
// adapter for relay authorization. It accepts only fixed-size capability
// digests; plaintext capabilities are outside this API by construction.
type MCPRelayStore struct {
	db *pgxpool.Pool
	q  *dbgen.Queries
}

// NewMCPRelayStore constructs the standalone relay persistence adapter.
func NewMCPRelayStore(db *pgxpool.Pool) *MCPRelayStore {
	return &MCPRelayStore{db: db, q: dbgen.New(db)}
}

func canonicalRuntimeRevisionMCPPolicy(raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		raw = []byte(emptyMCPPolicyJSON)
	}
	canonical, err := translator.CanonicalMCPPolicyJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize runtime revision MCP policy: %w", err)
	}
	return canonical, nil
}

func (s *MCPRelayStore) withTx(ctx context.Context, fn func(*dbgen.Queries) error) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin MCP relay transaction: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := fn(s.q.WithTx(tx)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit MCP relay transaction: %w", err)
	}
	return nil
}

// PersistMCPRelayGrant stores one pre-hashed grant. It locks and strictly
// validates the prepared revision policy, checks the exact binding, then
// inserts only when the AgentInstance currently references that revision.
func (s *MCPRelayStore) PersistMCPRelayGrant(
	ctx context.Context,
	digest mcprelay.CapabilityDigest,
	grant mcprelay.Grant,
) error {
	if digest == (mcprelay.CapabilityDigest{}) {
		return ErrMCPRelayGrantDigest
	}
	if grant.AgentInstanceID == "" || grant.Revision == "" || grant.BindingID == "" || grant.ExpiresAt.IsZero() ||
		!grant.ExpiresAt.After(time.Unix(0, 0).UTC()) ||
		(grant.RevokedAt != nil && grant.RevokedAt.IsZero()) {
		return ErrMCPRelayGrantScope
	}

	return s.withTx(ctx, func(q *dbgen.Queries) error {
		raw, err := q.LockRuntimeRevisionMCPPolicy(ctx, grant.Revision)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrMCPRelayGrantScope
		}
		if err != nil {
			return fmt.Errorf("lock MCP relay revision policy: %w", err)
		}
		policy, err := translator.DecodeMCPPolicyV1(raw)
		if err != nil {
			return fmt.Errorf("decode MCP relay revision policy: %w", err)
		}
		if _, found := policy.Binding(grant.BindingID); !found {
			return ErrMCPRelayGrantScope
		}

		_, err = q.InsertMCPRelayGrant(ctx, dbgen.InsertMCPRelayGrantParams{
			CapabilityHash: digest[:], AgentInstanceID: grant.AgentInstanceID,
			Revision: grant.Revision, BindingID: grant.BindingID,
			ExpiresAt: grant.ExpiresAt, RevokedAt: grant.RevokedAt,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrMCPRelayGrantScope
		}
		var postgresError *pgconn.PgError
		if errors.As(err, &postgresError) && postgresError.Code == "23505" && postgresError.ConstraintName == "mcp_relay_grant_pkey" {
			return ErrMCPRelayGrantConflict
		}
		if err != nil {
			return fmt.Errorf("persist MCP relay grant: %w", err)
		}
		return nil
	})
}

// RevokeMCPRelayGrant irreversibly records the first revocation timestamp.
func (s *MCPRelayStore) RevokeMCPRelayGrant(ctx context.Context, digest mcprelay.CapabilityDigest, revokedAt time.Time) error {
	if revokedAt.IsZero() {
		return fmt.Errorf("MCP relay revocation timestamp is required")
	}
	_, err := s.q.RevokeMCPRelayGrant(ctx, dbgen.RevokeMCPRelayGrantParams{
		CapabilityHash: digest[:], RevokedAt: &revokedAt,
	})
	if err != nil {
		return fmt.Errorf("revoke MCP relay grant: %w", notFoundOr(err))
	}
	return nil
}

// MCPPolicy loads and strictly validates private policy for one immutable
// prepared revision.
func (s *MCPRelayStore) MCPPolicy(ctx context.Context, revision string) (translator.MCPPolicyV1, error) {
	raw, err := s.q.GetRuntimeRevisionMCPPolicy(ctx, revision)
	if err != nil {
		return translator.MCPPolicyV1{}, fmt.Errorf("get MCP relay revision policy: %w", notFoundOr(err))
	}
	policy, err := translator.DecodeMCPPolicyV1(raw)
	if err != nil {
		return translator.MCPPolicyV1{}, fmt.Errorf("decode MCP relay revision policy: %w", err)
	}
	return policy, nil
}

// VerifyMCPGrant resolves a fixed-size capability digest. Expiry and revocation
// remain explicit returned lifecycle state for the relay Engine to enforce.
func (s *MCPRelayStore) VerifyMCPGrant(ctx context.Context, digest mcprelay.CapabilityDigest) (mcprelay.Grant, error) {
	row, err := s.q.GetMCPRelayGrant(ctx, digest[:])
	if err != nil {
		return mcprelay.Grant{}, fmt.Errorf("get MCP relay grant: %w", notFoundOr(err))
	}
	if len(row.CapabilityHash) != sha256.Size || !bytes.Equal(row.CapabilityHash, digest[:]) {
		return mcprelay.Grant{}, fmt.Errorf("stored MCP relay capability digest is inconsistent")
	}
	return mcprelay.Grant{
		AgentInstanceID: row.AgentInstanceID,
		Revision:        row.Revision,
		BindingID:       row.BindingID,
		ExpiresAt:       row.ExpiresAt,
		RevokedAt:       row.RevokedAt,
	}, nil
}

// MCPInstanceLifecycle reads the indexed AgentInstance columns that are the
// authoritative current lifecycle state. Any operation other than NONE fences
// relay access, including CREATE while an instance is still preparing.
func (s *MCPRelayStore) MCPInstanceLifecycle(ctx context.Context, id string) (mcprelay.InstanceLifecycle, error) {
	row, err := s.q.GetMCPRelayInstanceLifecycle(ctx, id)
	if err != nil {
		return mcprelay.InstanceLifecycle{}, fmt.Errorf("get MCP relay AgentInstance lifecycle: %w", notFoundOr(err))
	}
	state := mcprelay.InstanceState(row.State)
	switch state {
	case mcprelay.InstanceStateCreating, mcprelay.InstanceStateReady, mcprelay.InstanceStateSuspended, mcprelay.InstanceStateFailed:
	default:
		return mcprelay.InstanceLifecycle{}, fmt.Errorf("stored MCP relay AgentInstance state is invalid")
	}
	switch row.Operation {
	case "NONE", "CREATE", "SUSPEND", "RESUME", "DELETE":
	default:
		return mcprelay.InstanceLifecycle{}, fmt.Errorf("stored MCP relay AgentInstance operation is invalid")
	}
	return mcprelay.InstanceLifecycle{
		AgentInstanceID:  row.ID,
		PreparedRevision: derefStr(row.PreparedRevision),
		State:            state,
		OperationPending: row.Operation != "NONE",
	}, nil
}
