package tuyacloudlink

import (
	"aether/backend/internal/adapters/tuyacloud"
	"context"
	"errors"
	"sync"
	"time"
)

// tokenMargin is how long before its expiry the access token is renewed.
const tokenMargin = 5 * time.Minute

// session is one link's OpenAPI client with its token cache. The client is not safe for concurrent use, so every
// call holds the lock; each call Tuya bills (token calls included) is counted.
type session struct {
	mu     sync.Mutex
	client *tuyacloud.Client
	authed bool
	now    func() time.Time
	count  func(calls int64)
}

// ready makes sure the access token is valid for at least tokenMargin: the first call authenticates, a token
// about to expire is refreshed (or, when the refresh fails, obtained again).
func (s *session) ready(ctx context.Context) error {
	if s.authed && s.now().Add(tokenMargin).Before(s.client.ExpiresAt()) {
		return nil
	}
	if s.authed {
		s.count(1)
		if s.client.RefreshToken(ctx) == nil {
			return nil
		}
	}
	s.authed = false
	s.count(1)
	if e := s.client.Authenticate(ctx); e != nil {
		return e
	}
	s.authed = true
	return nil
}

// call runs fn with a valid token. A call refused for its token (expired early, revoked) authenticates again once.
func (s *session) call(ctx context.Context, fn func(*tuyacloud.Client) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.ready(ctx); e != nil {
		return e
	}
	s.count(1)
	e := fn(s.client)
	if errors.Is(e, tuyacloud.ErrAuth) {
		s.authed = false
		if e2 := s.ready(ctx); e2 != nil {
			return e2
		}
		s.count(1)
		e = fn(s.client)
	}
	return e
}
