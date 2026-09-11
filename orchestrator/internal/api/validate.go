package api

import "errors"

// minPasswordLength is deliberately a length floor rather than a composition
// rule (no forced symbol classes): length is what resists guessing, and
// composition rules push users toward predictable substitutions.
const minPasswordLength = 10

var errPasswordTooShort = errors.New("password must be at least 10 characters")

func validatePassword(password string) error {
	if len(password) < minPasswordLength {
		return errPasswordTooShort
	}
	return nil
}
