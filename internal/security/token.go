package security

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"math/big"
	"regexp"
	"strings"
)

// Join tokens look like "<id>.<secret>": the id names the stored token, the
// secret (about 82 bits) is only stored as a hash.
const (
	tokenIDLen     = 6
	tokenSecretLen = 16
	tokenAlphabet  = "abcdefghijklmnopqrstuvwxyz0123456789"
)

var tokenRE = regexp.MustCompile(`^[a-z0-9]{6}\.[a-z0-9]{16}$`)

// NewToken returns a random token id and secret.
func NewToken() (id, secret string, err error) {
	if id, err = randomString(tokenIDLen); err != nil {
		return "", "", err
	}
	if secret, err = randomString(tokenSecretLen); err != nil {
		return "", "", err
	}
	return id, secret, nil
}

// ParseToken splits "<id>.<secret>".
func ParseToken(token string) (id, secret string, err error) {
	if !tokenRE.MatchString(token) {
		return "", "", errors.New("malformed join token")
	}
	id, secret, _ = strings.Cut(token, ".")
	return id, secret, nil
}

// HashSecret returns the stored form of a token secret.
func HashSecret(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

// SecretMatches compares a secret with its stored hash in constant time.
func SecretMatches(hash []byte, secret string) bool {
	return subtle.ConstantTimeCompare(hash, HashSecret(secret)) == 1
}

func randomString(n int) (string, error) {
	b := make([]byte, n)
	max := big.NewInt(int64(len(tokenAlphabet)))
	for i := range b {
		v, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		b[i] = tokenAlphabet[v.Int64()]
	}
	return string(b), nil
}
