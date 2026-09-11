// Package auth holds first-party authentication primitives: password
// hashing and the signed session used by the OAuth authorize flow. The
// full user-facing auth API (login endpoints, JWT for /v1) builds on these
// in the identity phase.
package auth

import "golang.org/x/crypto/bcrypt"

const bcryptCost = 12

func HashPassword(plain string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(plain), bcryptCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

func VerifyPassword(hash, plain string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}
