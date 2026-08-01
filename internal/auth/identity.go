package auth

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Identity is what a resolved virtual key means for the rest of the
// gateway: which org/team it belongs to, which key was actually used, and
// the token-per-minute ceilings the rate limiter checks at each level.
// KeyTPMLimit is nil when the key has no override, meaning "use the
// gateway's configured default_tpm."
type Identity struct {
	OrgID         uuid.UUID
	TeamID        uuid.UUID
	KeyID         uuid.UUID
	AllowedModels []string
	OrgTPMLimit   int64
	TeamTPMLimit  int64
	KeyTPMLimit   *int64
}

var (
	ErrNotFound = errors.New("auth: key not found")
	ErrRevoked  = errors.New("auth: key revoked")
)

// Key is the full record behind a virtual key, as issued. Plaintext is
// only ever populated by IssueKey's return value — it is never read back
// from storage.
type Key struct {
	ID            uuid.UUID
	TeamID        uuid.UUID
	KeyPrefix     string
	Label         string
	AllowedModels []string
	TPMLimit      *int64
	RevokedAt     *time.Time
	CreatedAt     time.Time
}
