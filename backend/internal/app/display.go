package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"time"
	"unicode/utf8"

	"aether/backend/internal/domain"
	"aether/backend/internal/security"
	"github.com/google/uuid"
)

// Display links (docs/platform/display.md). A pairing code is typed on the TV (or opened from a link whose fragment
// carries it); the TV exchanges it once for a display token. Neither is stored in the clear.

// pairingAlphabet has no look-alikes (0/O, 1/I/L): 31 symbols, 8 of them are about 2^39.6 codes.
const pairingAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

func newPairingCode() string {
	b := make([]byte, 8)
	out := make([]byte, 0, 9)
	for i := 0; i < len(b); {
		if _, e := rand.Read(b[i : i+1]); e != nil {
			panic(e)
		}
		// Rejection sampling keeps every symbol equally likely (248 = 8 × 31).
		if b[i] >= 248 {
			continue
		}
		if i == 4 {
			out = append(out, '-')
		}
		out = append(out, pairingAlphabet[int(b[i])%len(pairingAlphabet)])
		i++
	}
	return string(out)
}

// NormalizePairingCode accepts what a person types: any case, spaces or dashes. Anything else is not a code.
func NormalizePairingCode(raw string) (string, bool) {
	if len(raw) > 32 {
		return "", false
	}
	var b strings.Builder
	for _, r := range strings.ToUpper(raw) {
		switch {
		case r == ' ' || r == '-':
		case strings.ContainsRune(pairingAlphabet, r):
			b.WriteRune(r)
		default:
			return "", false
		}
	}
	code := b.String()
	return code, len(code) == 8
}

func pairingDigest(code string) string { return security.Digest("display-pairing:" + code) }

// newDisplayToken is dsp_<32 random bytes, base64url>. It is looked up by its digest.
func newDisplayToken() string {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return domain.DisplayTokenPrefix + base64.RawURLEncoding.EncodeToString(b)
}

// IsDisplayToken reports whether a bearer credential is a display token (not a member's access token).
func IsDisplayToken(token string) bool { return strings.HasPrefix(token, domain.DisplayTokenPrefix) }

func validDisplayToken(token string) bool {
	rest, ok := strings.CutPrefix(token, domain.DisplayTokenPrefix)
	if !ok || len(rest) != 43 {
		return false
	}
	b, e := base64.RawURLEncoding.DecodeString(rest)
	return e == nil && len(b) == 32
}

func validDisplaySettings(s *domain.DisplaySettings) bool {
	s.Name = strings.TrimSpace(s.Name)
	if !utf8.ValidString(s.Name) || utf8.RuneCountInString(s.Name) < 1 || utf8.RuneCountInString(s.Name) > 80 || strings.ContainsAny(s.Name, "\x00\r\n") {
		return false
	}
	if len(s.ProjectIDs) > 50 {
		return false
	}
	seen := map[string]bool{}
	for _, id := range s.ProjectIDs {
		if !security.ValidID(id) || seen[id] {
			return false
		}
		seen[id] = true
	}
	if s.ProjectIDs == nil {
		s.ProjectIDs = []string{}
	}
	return domain.ValidPlaylist(s.Playlist)
}

// CreateDisplay adds a display and returns its first pairing code (shown once).
func (s *Service) CreateDisplay(ctx context.Context, p domain.Principal, in domain.DisplaySettings) (domain.DisplayPairing, error) {
	if !p.CanManageDevices() {
		return domain.DisplayPairing{}, domain.ErrForbidden
	}
	if in.Playlist == nil {
		in.Playlist = domain.DefaultPlaylist()
	}
	if !validDisplaySettings(&in) {
		return domain.DisplayPairing{}, domain.ErrInvalid
	}
	code := newPairingCode()
	expires := time.Now().UTC().Add(domain.DisplayPairingTTL)
	d, e := s.Repo.CreateDisplay(ctx, p, uuid.NewString(), in, pairingDigest(strings.ReplaceAll(code, "-", "")), expires)
	if e != nil {
		return domain.DisplayPairing{}, e
	}
	return domain.DisplayPairing{Display: d, Code: code, ExpiresAt: expires}, nil
}

func (s *Service) UpdateDisplay(ctx context.Context, p domain.Principal, id string, in domain.DisplaySettings) (domain.Display, error) {
	if !p.CanManageDevices() {
		return domain.Display{}, domain.ErrForbidden
	}
	if !security.ValidID(id) || !validDisplaySettings(&in) {
		return domain.Display{}, domain.ErrInvalid
	}
	return s.Repo.UpdateDisplay(ctx, p, id, in)
}

// RepairDisplay issues a new pairing code; the display's current token stops working at once.
func (s *Service) RepairDisplay(ctx context.Context, p domain.Principal, id string) (domain.DisplayPairing, error) {
	if !p.CanManageDevices() {
		return domain.DisplayPairing{}, domain.ErrForbidden
	}
	if !security.ValidID(id) {
		return domain.DisplayPairing{}, domain.ErrInvalid
	}
	code := newPairingCode()
	expires := time.Now().UTC().Add(domain.DisplayPairingTTL)
	d, e := s.Repo.RepairDisplay(ctx, p, id, pairingDigest(strings.ReplaceAll(code, "-", "")), expires)
	if e != nil {
		return domain.DisplayPairing{}, e
	}
	return domain.DisplayPairing{Display: d, Code: code, ExpiresAt: expires}, nil
}

func (s *Service) RevokeDisplay(ctx context.Context, p domain.Principal, id string) error {
	if !p.CanManageDevices() {
		return domain.ErrForbidden
	}
	if !security.ValidID(id) {
		return domain.ErrInvalid
	}
	return s.Repo.RevokeDisplay(ctx, p, id)
}

// PairDisplay exchanges a pairing code for a display token (returned once; only its digest is kept).
func (s *Service) PairDisplay(ctx context.Context, raw, ip string) (string, domain.DisplaySession, error) {
	code, ok := NormalizePairingCode(raw)
	if !ok {
		return "", domain.DisplaySession{}, domain.ErrUnauthorized
	}
	token := newDisplayToken()
	if _, _, e := s.Repo.PairDisplay(ctx, pairingDigest(code), security.Digest(token)); e != nil {
		return "", domain.DisplaySession{}, e
	}
	session, e := s.Repo.DisplaySession(ctx, security.Digest(token), ip)
	if e != nil {
		return "", domain.DisplaySession{}, e
	}
	return token, session, nil
}

// AuthenticateDisplay resolves a display token to its display and current settings.
func (s *Service) AuthenticateDisplay(ctx context.Context, token, ip string) (domain.DisplaySession, error) {
	if !validDisplayToken(token) {
		return domain.DisplaySession{}, domain.ErrUnauthorized
	}
	return s.Repo.DisplaySession(ctx, security.Digest(token), ip)
}
