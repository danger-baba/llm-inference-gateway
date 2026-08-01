package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store issues, revokes, and resolves virtual keys against PostgreSQL.
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// IssueKey generates a new key, stores only its hash and display prefix,
// and returns the plaintext exactly once — this is the only place it ever
// exists outside the client that receives it.
func (s *Store) IssueKey(ctx context.Context, teamID uuid.UUID, label string, allowedModels []string, tpmLimit *int64) (plaintext string, key Key, err error) {
	plaintext, err = generateKey()
	if err != nil {
		return "", Key{}, err
	}
	hash := hashKey(plaintext)
	prefix := displayPrefix(plaintext)

	row := s.pool.QueryRow(ctx, `
		INSERT INTO virtual_keys (team_id, key_hash, key_prefix, label, allowed_models, tpm_limit)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, created_at
	`, teamID, hash, prefix, label, allowedModels, tpmLimit)

	var k Key
	if err := row.Scan(&k.ID, &k.CreatedAt); err != nil {
		return "", Key{}, fmt.Errorf("auth: issue key: %w", err)
	}
	k.TeamID = teamID
	k.KeyPrefix = prefix
	k.Label = label
	k.AllowedModels = allowedModels
	k.TPMLimit = tpmLimit
	return plaintext, k, nil
}

// RevokeKey sets revoked_at (never deletes, so ledger history referencing
// this key survives) and returns the key's hash so the caller can also
// invalidate its cache entry. Revoking an already-revoked or nonexistent
// key returns ErrNotFound.
func (s *Store) RevokeKey(ctx context.Context, id uuid.UUID) ([]byte, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE virtual_keys SET revoked_at = now()
		WHERE id = $1 AND revoked_at IS NULL
		RETURNING key_hash
	`, id)

	var hash []byte
	if err := row.Scan(&hash); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("auth: revoke key: %w", err)
	}
	return hash, nil
}

// Resolve looks up the identity behind a key's hash. revoked is reported
// separately from err so a caller (the cache, in particular) can still
// learn — and cache — who a revoked key belonged to, rather than treating
// "revoked" the same as "never existed."
func (s *Store) Resolve(ctx context.Context, hash []byte) (id Identity, revoked bool, err error) {
	row := s.pool.QueryRow(ctx, `
		SELECT vk.id, vk.team_id, t.org_id, vk.allowed_models, vk.revoked_at
		FROM virtual_keys vk
		JOIN teams t ON t.id = vk.team_id
		WHERE vk.key_hash = $1
	`, hash)

	var revokedAt *time.Time
	if err := row.Scan(&id.KeyID, &id.TeamID, &id.OrgID, &id.AllowedModels, &revokedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Identity{}, false, ErrNotFound
		}
		return Identity{}, false, fmt.Errorf("auth: resolve key: %w", err)
	}
	return id, revokedAt != nil, nil
}
