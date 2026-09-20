// Package account is the lifecycle of a Ferry account: creating one from a
// Resend API key, generating the app password Apple Mail logs in with, and
// authenticating IMAP and SMTP sessions against it.
package account

import (
	"crypto/rand"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// bcryptCost is deliberately above the library default. A login happens once
// per connection, not per request, so a few tens of milliseconds is invisible
// to the user and expensive for anyone who obtains the database.
const bcryptCost = 12

// appPasswordAlphabet excludes characters that are easy to misread when the
// password is typed into Apple Mail by hand.
const appPasswordAlphabet = "abcdefghijkmnopqrstuvwxyz23456789"

// appPasswordGroups and appPasswordGroupLen give a 20-character password in
// five dash-separated groups, which is about 98 bits of entropy.
const (
	appPasswordGroups   = 5
	appPasswordGroupLen = 4
)

// GenerateAppPassword returns a new app password in the form
// "abcd-efgh-ijkl-mnop-qrst". It is shown to the user once and never stored in
// the clear.
func GenerateAppPassword() (string, error) {
	groups := make([]string, appPasswordGroups)
	for g := range groups {
		buf := make([]byte, appPasswordGroupLen)
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("account: generate password: %w", err)
		}
		chars := make([]byte, appPasswordGroupLen)
		for i, b := range buf {
			// The alphabet length divides evenly into 256 only by luck, so
			// take the value modulo its length and accept the negligible bias
			// that remains at 33 symbols over 98 bits.
			chars[i] = appPasswordAlphabet[int(b)%len(appPasswordAlphabet)]
		}
		groups[g] = string(chars)
	}
	return strings.Join(groups, "-"), nil
}

// HashPassword hashes an app password for storage.
func HashPassword(password string) (string, error) {
	if password == "" {
		return "", errors.New("account: empty password")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return "", fmt.Errorf("account: hash password: %w", err)
	}
	return string(h), nil
}

// VerifyPassword checks a password against a stored hash.
func VerifyPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// dummyHash is compared against when an account does not exist, so that a
// failed login costs the same whether or not the name is real. Without it the
// response time alone would enumerate account names.
var dummyHash = func() []byte {
	h, err := bcrypt.GenerateFromPassword([]byte("ferry-dummy-password"), bcryptCost)
	if err != nil {
		panic("account: cannot initialise bcrypt: " + err.Error())
	}
	return h
}()
