package security

import (
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"fmt"
	"golang.org/x/crypto/pbkdf2"
)

// Mosquitto 2 password-file format. A random 256-bit credential is shown once.
func MQTTHash(password string) (string, error) {
	salt := make([]byte, 12)
	if _, e := rand.Read(salt); e != nil {
		return "", e
	}
	const iterations = 100000
	hash := pbkdf2.Key([]byte(password), salt, iterations, 64, sha512.New)
	return fmt.Sprintf("$7$%d$%s$%s", iterations, base64.StdEncoding.EncodeToString(salt), base64.StdEncoding.EncodeToString(hash)), nil
}
