package tuyalocal

import (
	"crypto/hmac"
	"errors"
)

// ErrKeyRejected means the device answered the 3.4/3.5 session-key negotiation with an HMAC that does not
// match our local key: the key is wrong (typically the device was re-paired in the app, which issues a new
// one). It is definitive, unlike the 3.3 case (see ErrKeySuspect).
var ErrKeyRejected = errors.New("tuyalocal: device rejected the local key")

// nonceLen is the size of both negotiation nonces.
const nonceLen = 16

// negotiation holds the client side of the 3.4/3.5 session-key exchange (XenonDevice._negotiate_session_key):
//
//	client → SESS_KEY_NEG_START  local_nonce
//	device → SESS_KEY_NEG_RESP   remote_nonce ‖ HMAC-SHA256(local_key, local_nonce)
//	client → SESS_KEY_NEG_FINISH HMAC-SHA256(local_key, remote_nonce)
//
// and the session key is local_nonce XOR remote_nonce encrypted with the local key: AES-ECB on 3.4, and on
// 3.5 AES-GCM with iv = local_nonce[:12], keeping only the 16 ciphertext bytes (…_generate_finalize).
type negotiation struct {
	version     Version
	localKey    []byte
	localNonce  []byte
	remoteNonce []byte
}

// finishPayload checks the device's answer and returns the FINISH payload. resp is the decrypted RESP
// payload (Session.Decode already removed the 3.4 ECB layer or the 3.5 GCM framing).
func (n *negotiation) finishPayload(resp []byte) ([]byte, error) {
	if len(resp) < nonceLen+hmacLen {
		return nil, ErrKeyRejected
	}
	if !hmac.Equal(resp[nonceLen:nonceLen+hmacLen], hmacSHA256(n.localKey, n.localNonce)) {
		return nil, ErrKeyRejected
	}
	n.remoteNonce = append([]byte{}, resp[:nonceLen]...)
	return hmacSHA256(n.localKey, n.remoteNonce), nil
}

// sessionKey derives the key used for the rest of the connection.
func (n *negotiation) sessionKey() ([]byte, error) {
	return DeriveSessionKey(n.version, n.localKey, n.localNonce, n.remoteNonce)
}

// DeriveSessionKey computes the 3.4/3.5 session key from both nonces. Exported for the simulator, which has
// to derive the same key on the device side.
func DeriveSessionKey(v Version, localKey, localNonce, remoteNonce []byte) ([]byte, error) {
	if len(localNonce) != nonceLen || len(remoteNonce) != nonceLen {
		return nil, errors.New("tuyalocal: negotiation nonces must be 16 bytes")
	}
	x := make([]byte, nonceLen)
	for i := range x {
		x[i] = localNonce[i] ^ remoteNonce[i]
	}
	if v == V34 {
		return ecbEncrypt(localKey, x, false)
	}
	sealed, e := gcmSeal(localKey, localNonce[:ivLen], x, nil)
	if e != nil {
		return nil, e
	}
	return sealed[:nonceLen], nil
}

// NegotiationResponse builds the RESP payload a device sends (used by the simulator and tests).
func NegotiationResponse(localKey, localNonce, remoteNonce []byte) []byte {
	return append(append([]byte{}, remoteNonce...), hmacSHA256(localKey, localNonce)...)
}

// VerifyFinish checks a FINISH payload on the device side.
func VerifyFinish(localKey, remoteNonce, finish []byte) bool {
	return len(finish) >= hmacLen && hmac.Equal(finish[:hmacLen], hmacSHA256(localKey, remoteNonce))
}
