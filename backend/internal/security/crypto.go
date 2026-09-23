package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
)

// DeriveKey produces a purpose-bound 32-byte key from the deployment signing key so different
// secrets (channel tokens, …) never share a raw key with JWT signing.
func DeriveKey(master []byte, purpose string) []byte {
	sum := sha256.Sum256(append(append([]byte("aether/"+purpose+"/"), 0), master...))
	return sum[:]
}

// OpenAny tries each key in order. Deployments that introduce a dedicated seal key keep reading secrets that
// were sealed with the older derived key.
func OpenAny(sealed string, keys ...[]byte) (string, error) {
	var last error = errors.New("no key")
	for _, k := range keys {
		if len(k) == 0 {
			continue
		}
		out, e := Open(k, sealed)
		if e == nil {
			return out, nil
		}
		last = e
	}
	return "", last
}

// Seal encrypts a short secret with AES-256-GCM; output is base64(nonce||ciphertext).
func Seal(key []byte, plaintext string) (string, error) {
	block, e := aes.NewCipher(key)
	if e != nil {
		return "", e
	}
	gcm, e := cipher.NewGCM(block)
	if e != nil {
		return "", e
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, e = rand.Read(nonce); e != nil {
		return "", e
	}
	return base64.StdEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte(plaintext), nil)), nil
}

// Open reverses Seal.
func Open(key []byte, sealed string) (string, error) {
	raw, e := base64.StdEncoding.DecodeString(sealed)
	if e != nil {
		return "", e
	}
	block, e := aes.NewCipher(key)
	if e != nil {
		return "", e
	}
	gcm, e := cipher.NewGCM(block)
	if e != nil {
		return "", e
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("sealed value too short")
	}
	out, e := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if e != nil {
		return "", e
	}
	return string(out), nil
}
