package console

import (
	"crypto/rand"
	"encoding/hex"
)

// randomHex returns n random bytes, hex-encoded. Used by generateToken and
// by session/CSRF token generation.
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
