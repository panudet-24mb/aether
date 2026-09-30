package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"

	"golang.org/x/crypto/nacl/box"
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

// TuyaCloudBinding ties sealed Tuya Cloud credentials to one tenant and gateway. It is sealed inside the box with
// the credentials and checked when the box is opened, so a sealed value copied onto another row does not open there.
func TuyaCloudBinding(tenant, gateway string) string {
	return "tuya-cloud\x00" + tenant + "\x00" + gateway
}

// TuyaCloudSealed is what a Tuya Cloud box holds: the binding and the project's Access ID and Secret.
type TuyaCloudSealed struct {
	Binding      string `json:"bind"`
	AccessID     string `json:"id"`
	AccessSecret string `json:"secret"`
}

// ParseTuyaCloudKey reads a base64 X25519 key (32 bytes), public or private.
func ParseTuyaCloudKey(raw string) (*[32]byte, error) {
	b, e := base64.StdEncoding.DecodeString(raw)
	if e != nil || len(b) != 32 {
		return nil, errors.New("a Tuya Cloud key must be 32 base64-encoded bytes")
	}
	var k [32]byte
	copy(k[:], b)
	return &k, nil
}

// SealTuyaCloud seals a project's Access ID and Secret for one tenant and gateway in an anonymous box
// (X25519 + XSalsa20-Poly1305, nacl/box SealAnonymous) to the tuya-cloud worker's public key. The API holds only
// that public key: nothing it holds can open the result (package tuyacloudlink/cloudkeys, in the worker, can).
func SealTuyaCloud(public *[32]byte, tenant, gateway, accessID, accessSecret string) (string, error) {
	if public == nil {
		return "", errors.New("no Tuya Cloud public key")
	}
	b, e := json.Marshal(TuyaCloudSealed{Binding: TuyaCloudBinding(tenant, gateway), AccessID: accessID, AccessSecret: accessSecret})
	if e != nil {
		return "", e
	}
	out, e := box.SealAnonymous(nil, b, public, rand.Reader)
	if e != nil {
		return "", e
	}
	return base64.StdEncoding.EncodeToString(out), nil
}

// AccessIDDigest identifies a Tuya Cloud project without storing its Access ID: one project can feed one gateway.
func AccessIDDigest(accessID string) string {
	sum := sha256.Sum256([]byte("aether/tuya-cloud-access-id\x00" + accessID))
	return hex.EncodeToString(sum[:])
}
