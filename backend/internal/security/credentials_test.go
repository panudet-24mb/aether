package security

import (
	"aether/backend/internal/domain"
	"encoding/base64"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"strings"
	"testing"
	"time"
)

func TestPasswords(t *testing.T) {
	password := "correct horse battery staple"
	a, e := HashPassword(password)
	if e != nil {
		t.Fatal(e)
	}
	b, e := HashPassword(password)
	if e != nil {
		t.Fatal(e)
	}
	if a == b {
		t.Fatal("salt must differ")
	}
	if !CheckPassword(password, a) || CheckPassword("wrong password", a) {
		t.Fatal("password verification")
	}
	for _, bad := range []string{"", strings.Replace(a, "m=65536", "m=999999999", 1), "$argon2id$v=19$m=65536,t=3,p=2$bad$bad"} {
		if CheckPassword(password, bad) {
			t.Fatal("accepted malformed or unbounded hash")
		}
	}
	if _, e = HashPassword("short"); e == nil {
		t.Fatal("short password accepted")
	}
	if _, e = HashPassword(strings.Repeat("a", 129)); e == nil {
		t.Fatal("long password accepted")
	}
}
func TestJWTBoundaries(t *testing.T) {
	key := []byte(strings.Repeat("k", 48))
	tokens := NewTokens(key, "aether")
	now := time.Now()
	s := domain.Session{ID: uuid.NewString(), UserID: uuid.NewString(), TenantID: uuid.NewString(), ExpiresAt: now.Add(time.Hour)}
	raw, e := tokens.Access(s, now)
	if e != nil {
		t.Fatal(e)
	}
	p, e := tokens.Verify(raw)
	if e != nil || p.UserID != s.UserID || p.TenantID != s.TenantID {
		t.Fatal("valid token rejected", e)
	}
	if _, e = NewTokens([]byte(strings.Repeat("x", 48)), "aether").Verify(raw); e == nil {
		t.Fatal("wrong signature accepted")
	}
	for _, kind := range []string{"expired", "issuer", "audience", "no-expiry", "none", "bad-tenant"} {
		t.Run(kind, func(t *testing.T) {
			c := Claims{TenantID: s.TenantID, SessionID: s.ID, RegisteredClaims: jwt.RegisteredClaims{Subject: s.UserID, Issuer: "aether", Audience: jwt.ClaimStrings{"aether-api"}, ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour))}}
			method := jwt.SigningMethod(jwt.SigningMethodHS256)
			var signingKey any = key
			switch kind {
			case "expired":
				c.ExpiresAt = jwt.NewNumericDate(now.Add(-time.Hour))
			case "issuer":
				c.Issuer = "other"
			case "audience":
				c.Audience = jwt.ClaimStrings{"other"}
			case "no-expiry":
				c.ExpiresAt = nil
			case "none":
				method = jwt.SigningMethodNone
				signingKey = jwt.UnsafeAllowNoneSignatureType
			case "bad-tenant":
				c.TenantID = "not-a-uuid"
			}
			raw, e := jwt.NewWithClaims(method, c).SignedString(signingKey)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = tokens.Verify(raw); e == nil {
				t.Fatal("unsafe token accepted")
			}
		})
	}
}
func TestRefreshShape(t *testing.T) {
	user := uuid.NewString()
	raw := NewRefresh(user)
	if got, e := RefreshUser(raw); e != nil || got != user {
		t.Fatal(got, e)
	}
	if len(Digest(raw)) != 64 {
		t.Fatal("digest length")
	}
	parts := strings.Split(raw, ".")
	if b, e := base64.RawURLEncoding.DecodeString(parts[1]); e != nil || len(b) != 32 {
		t.Fatal("insufficient entropy")
	}
	for _, bad := range []string{"", user + ".short", "invalid." + RandomToken(), raw + ".extra"} {
		if _, e := RefreshUser(bad); e == nil {
			t.Fatal("bad refresh accepted")
		}
	}
}
