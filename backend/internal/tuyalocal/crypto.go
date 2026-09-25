// Package tuyalocal speaks the Tuya local LAN protocol (versions 3.1, 3.3, 3.4 and 3.5) so Aether Edge can
// read and command Tuya Wi‑Fi devices without the Tuya cloud. It uses only the Go standard library.
//
// The wire format is not published by Tuya. Everything here is ported from tinytuya (MIT, © Jason Cox,
// see LICENSE.tinytuya), version 1.20.0: tinytuya/core/{header,command_types,message_helper,crypto_helper,
// XenonDevice,udp_helper}.py and PROTOCOL.md. Comments cite the tinytuya function a rule comes from.
package tuyalocal

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"errors"
	"hash/crc32"
)

// ErrPadding reports ECB plaintext whose PKCS#7 padding is invalid: on 3.1–3.4 that almost always means the
// frame was encrypted with a different key.
var ErrPadding = errors.New("tuyalocal: invalid padding")

// udpKey decrypts LAN discovery broadcasts (udp_helper.py: udpkey = md5(b"yGAdlopoPVldABfn").digest()).
var udpKey = func() []byte { k := md5.Sum([]byte("yGAdlopoPVldABfn")); return k[:] }()

// ecbEncrypt is AES-128-ECB with optional PKCS#7 padding (crypto_helper.py encrypt without iv).
func ecbEncrypt(key, plain []byte, pad bool) ([]byte, error) {
	block, e := aes.NewCipher(key)
	if e != nil {
		return nil, e
	}
	if pad {
		n := aes.BlockSize - len(plain)%aes.BlockSize
		plain = append(append([]byte{}, plain...), bytes.Repeat([]byte{byte(n)}, n)...)
	}
	if len(plain)%aes.BlockSize != 0 {
		return nil, errors.New("tuyalocal: ECB plaintext is not a multiple of the block size")
	}
	out := make([]byte, len(plain))
	for i := 0; i < len(plain); i += aes.BlockSize {
		block.Encrypt(out[i:i+aes.BlockSize], plain[i:i+aes.BlockSize])
	}
	return out, nil
}

// ecbDecrypt reverses ecbEncrypt. With unpad it strips PKCS#7 padding and, unlike tinytuya (which only
// checks the length byte by default), verifies every padding byte: a wrong key is then detected instead of
// yielding garbage JSON.
func ecbDecrypt(key, ct []byte, unpad bool) ([]byte, error) {
	if len(ct) == 0 || len(ct)%aes.BlockSize != 0 {
		return nil, ErrPadding
	}
	block, e := aes.NewCipher(key)
	if e != nil {
		return nil, e
	}
	out := make([]byte, len(ct))
	for i := 0; i < len(ct); i += aes.BlockSize {
		block.Decrypt(out[i:i+aes.BlockSize], ct[i:i+aes.BlockSize])
	}
	if !unpad {
		return out, nil
	}
	n := int(out[len(out)-1])
	if n < 1 || n > aes.BlockSize || n > len(out) {
		return nil, ErrPadding
	}
	for _, b := range out[len(out)-n:] {
		if int(b) != n {
			return nil, ErrPadding
		}
	}
	return out[:len(out)-n], nil
}

// gcmSeal returns ciphertext||tag (16-byte tag, 12-byte nonce), as crypto_helper.py encrypt(iv=...) minus
// the leading IV, which the frame carries separately.
func gcmSeal(key, iv, plain, aad []byte) ([]byte, error) {
	aead, e := newGCM(key)
	if e != nil {
		return nil, e
	}
	return aead.Seal(nil, iv, plain, aad), nil
}

// gcmOpen verifies and decrypts ciphertext||tag.
func gcmOpen(key, iv, sealed, aad []byte) ([]byte, error) {
	aead, e := newGCM(key)
	if e != nil {
		return nil, e
	}
	return aead.Open(nil, iv, sealed, aad)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, e := aes.NewCipher(key)
	if e != nil {
		return nil, e
	}
	return cipher.NewGCM(block)
}

func hmacSHA256(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return m.Sum(nil)
}

func crc(data []byte) uint32 { return crc32.ChecksumIEEE(data) }
