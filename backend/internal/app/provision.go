package app

import (
	"aether/backend/internal/domain"
	"aether/backend/internal/ports"
	"aether/backend/internal/security"
	"context"
	"errors"
	"fmt"
)

// ProvisionOwner creates a workspace with its owner for the local admin command. The first one (newWorkspace false)
// is the one-time bootstrap, refused once any account exists. A further one makes the same account for another
// workspace; the registration switch it needs lives only in this Service, the API's public registration stays as
// configured. An email that already has an account is refused.
func ProvisionOwner(ctx context.Context, repo ports.Repository, newWorkspace bool, email, password, name, tenant string) (domain.Account, error) {
	s, e := New(repo, security.NewTokens([]byte(security.RandomToken()), "aether"), newWorkspace)
	if e != nil {
		return domain.Account{}, e
	}
	a, e := s.Register(ctx, email, password, name, tenant, !newWorkspace)
	switch {
	case errors.Is(e, domain.ErrConflict) && newWorkspace:
		return a, fmt.Errorf("an account with this email already exists (one removed from every workspace stays until `admin erase-user`): %w", e)
	case errors.Is(e, domain.ErrConflict):
		return a, fmt.Errorf("accounts already exist and bootstrap is one-time; use create-workspace for another workspace: %w", e)
	case errors.Is(e, domain.ErrInvalid):
		return a, fmt.Errorf("invalid email, name or workspace name, or a password outside 12-128 bytes: %w", e)
	}
	return a, e
}
