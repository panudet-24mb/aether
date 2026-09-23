package security

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"aether/backend/internal/domain"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"golang.org/x/crypto/argon2"
)

const memory = 64 * 1024

func HashPassword(password string) (string, error) {
	if len(password) < 12 || len(password) > 128 {
		return "", domain.ErrInvalid
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hash := argon2.IDKey([]byte(password), salt, 3, memory, 2, 32)
	return fmt.Sprintf("$argon2id$v=19$m=65536,t=3,p=2$%s$%s", base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(hash)), nil
}
func CheckPassword(password, encoded string) bool {
	if len(password) > 128 {
		return false
	}
	parts := strings.Split(encoded, "$")
	// Fixed, bounded parameters prevent a corrupt hash from exhausting CPU/memory.
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" || parts[3] != "m=65536,t=3,p=2" {
		return false
	}
	salt, e := base64.RawStdEncoding.DecodeString(parts[4])
	if e != nil || len(salt) != 16 {
		return false
	}
	want, e := base64.RawStdEncoding.DecodeString(parts[5])
	if e != nil || len(want) != 32 {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, 3, memory, 2, 32)
	return subtle.ConstantTimeCompare(got, want) == 1
}
func RandomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("secure randomness unavailable")
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func Digest(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }
func ValidID(s string) bool {
	v, e := uuid.Parse(s)
	return e == nil && v != uuid.Nil && v.String() == s
}

type Claims struct {
	TenantID  string `json:"tid"`
	SessionID string `json:"sid"`
	jwt.RegisteredClaims
}
type Tokens struct {
	key    []byte
	issuer string
}

func NewTokens(key []byte, issuer string) *Tokens { return &Tokens{key: key, issuer: issuer} }

// DeriveKey exposes a purpose-bound key without exposing the signing key itself.
func (t *Tokens) DeriveKey(purpose string) []byte { return DeriveKey(t.key, purpose) }
func (t *Tokens) Access(s domain.Session, now time.Time) (string, error) {
	exp := now.Add(15 * time.Minute)
	if s.ExpiresAt.Before(exp) {
		exp = s.ExpiresAt
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{TenantID: s.TenantID, SessionID: s.ID, RegisteredClaims: jwt.RegisteredClaims{Subject: s.UserID, Issuer: t.issuer, Audience: jwt.ClaimStrings{"aether-api"}, ExpiresAt: jwt.NewNumericDate(exp), IssuedAt: jwt.NewNumericDate(now), NotBefore: jwt.NewNumericDate(now), ID: uuid.NewString()}}).SignedString(t.key)
}
func (t *Tokens) Verify(raw string) (domain.Principal, error) {
	var c Claims
	tok, e := jwt.ParseWithClaims(raw, &c, func(tok *jwt.Token) (any, error) { return t.key, nil }, jwt.WithValidMethods([]string{"HS256"}), jwt.WithIssuer(t.issuer), jwt.WithAudience("aether-api"), jwt.WithExpirationRequired(), jwt.WithIssuedAt(), jwt.WithLeeway(5*time.Second))
	if e != nil || !tok.Valid || !ValidID(c.Subject) || !ValidID(c.TenantID) || !ValidID(c.SessionID) {
		return domain.Principal{}, domain.ErrUnauthorized
	}
	return domain.Principal{UserID: c.Subject, TenantID: c.TenantID, SessionID: c.SessionID}, nil
}
func NewRefresh(user string) string { return user + "." + RandomToken() }
func RefreshUser(raw string) (string, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 2 || !ValidID(parts[0]) || len(parts[1]) != 43 {
		return "", domain.ErrUnauthorized
	}
	if b, e := base64.RawURLEncoding.DecodeString(parts[1]); e != nil || len(b) != 32 {
		return "", domain.ErrUnauthorized
	}
	return parts[0], nil
}
func ParseLimit(s string, def int) (int, error) {
	if s == "" {
		return def, nil
	}
	n, e := strconv.Atoi(s)
	if e != nil || n < 1 || n > 1000 {
		return 0, domain.ErrInvalid
	}
	return n, nil
}
