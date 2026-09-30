// Package cloudkeys opens sealed Tuya Cloud project credentials. Only the tuya-cloud worker imports it and only
// the worker holds the private key (TUYA_CLOUD_PRIVATE_KEY): the API seals to the matching public key
// (security.SealTuyaCloud) and can never read a credential back. A test holds the import rule by listing every
// other binary's dependencies.
package cloudkeys

import (
	"aether/backend/internal/security"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"

	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/nacl/box"
)

// ErrSealed is any sealed value that does not open: wrong key, wrong tenant or gateway, or damaged.
var ErrSealed = errors.New("tuya cloud credentials do not open")

// Credentials are one project's Access ID and Secret, in the clear. Never log them.
type Credentials struct {
	AccessID     string
	AccessSecret string
}

// Key is the worker's private key with its public half.
type Key struct {
	private, public [32]byte
}

// ParseKey reads the worker's private key (base64, 32 bytes) and derives its public key.
func ParseKey(raw string) (*Key, error) {
	priv, e := security.ParseTuyaCloudKey(raw)
	if e != nil {
		return nil, e
	}
	return NewKey(priv)
}

// NewKey derives the public half of a private key.
func NewKey(private *[32]byte) (*Key, error) {
	pub, e := curve25519.X25519(private[:], curve25519.Basepoint)
	if e != nil {
		return nil, e
	}
	k := &Key{private: *private}
	copy(k.public[:], pub)
	return k, nil
}

// Public is the key the API seals to (TUYA_CLOUD_PUBLIC_KEY).
func (k *Key) Public() *[32]byte {
	p := k.public
	return &p
}

// Open opens credentials sealed for this tenant and gateway, trying each key in order (the current key, then a
// retired one during a rotation). A box whose binding names another tenant or gateway does not open.
func Open(tenant, gateway, sealed string, keys ...*Key) (Credentials, error) {
	raw, e := base64.StdEncoding.DecodeString(sealed)
	if e != nil {
		return Credentials{}, ErrSealed
	}
	want := security.TuyaCloudBinding(tenant, gateway)
	for _, k := range keys {
		if k == nil {
			continue
		}
		plain, ok := box.OpenAnonymous(nil, raw, &k.public, &k.private)
		if !ok {
			continue
		}
		var s security.TuyaCloudSealed
		if json.Unmarshal(plain, &s) != nil || subtle.ConstantTimeCompare([]byte(s.Binding), []byte(want)) != 1 || s.AccessID == "" || s.AccessSecret == "" {
			return Credentials{}, ErrSealed
		}
		return Credentials{AccessID: s.AccessID, AccessSecret: s.AccessSecret}, nil
	}
	return Credentials{}, ErrSealed
}
