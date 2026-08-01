package auth

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Identity is what a resolved virtual key means for the rest of the
// gateway: which org/team it belongs to, and which key was actually used.
type Identity struct {
	OrgID         uuid.UUID
	TeamID        uuid.UUID
	KeyID         uuid.UUID
	AllowedModels []string
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
