package auth

import (
	"crypto/rand"
	"crypto/sha256"
)

// appPasswordLen matches the token length Nextcloud itself issues. Clients
// treat the value as opaque, but staying the same shape avoids surprising any
// client that assumes it.
const appPasswordLen = 72

// tokenCharset is deliberately alphanumeric. The token travels in a Basic auth
// header and, for some mobile clients, inside a custom URL scheme, so avoiding
// punctuation removes any question of escaping.
const tokenCharset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// GenerateAppPassword returns a new random device token.
func GenerateAppPassword() (string, error) {
	// 62 does not divide 256, so mapping raw bytes with a modulo would skew the
	// distribution. Reject bytes in the biased tail instead.
	const maxUnbiased = 256 - (256 % len(tokenCharset)) // 248

	out := make([]byte, 0, appPasswordLen)
	buf := make([]byte, appPasswordLen)
	for len(out) < appPasswordLen {
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		for _, b := range buf {
			if int(b) >= maxUnbiased {
				continue
			}
			out = append(out, tokenCharset[int(b)%len(tokenCharset)])
			if len(out) == appPasswordLen {
				break
			}
		}
	}
	return string(out), nil
}

// HashToken returns the value stored for an app password.
//
// This is a plain SHA-256 rather than a slow KDF on purpose. App passwords are
// 72 random alphanumeric characters, so there is no dictionary to attack and
// nothing for key stretching to buy; meanwhile every WebDAV request carries one,
// and an argon2 verification per request would dominate sync latency.
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}
