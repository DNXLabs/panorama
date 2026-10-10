// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"strings"
)

// Prefixes for the credentials Panorama issues.
//
// Every one is distinguishable at a glance, which is the point: a value in a
// log, a paste, or a support ticket should identify what it is without anyone
// having to try it. Secret scanners key on exactly this.
const (
	// SessionPrefix marks a console session cookie.
	SessionPrefix = "pns_"
	// SetupCodePrefix marks the one-time code that opens a setup session. It
	// is what a deployment is configured with before anybody can sign in, and
	// what repairs one nobody can sign in to any more. There is one such code
	// at a time, and it creates no account.
	SetupCodePrefix = "pnb_"
	// SetupSessionPrefix marks the cookie a redeemed setup code sets. A
	// separate prefix from a console session so the authentication path can
	// tell them apart before it touches the database, and so the two can never
	// be confused for one another in a log.
	SetupSessionPrefix = "pnx_"
	// ServicePrefix marks a team-owned service token - what a CI job holds.
	// It authenticates at the gateway exactly as a personal token does, but it
	// bills a team rather than a person, and a separate prefix is what lets the
	// authentication path pick the one table to look in before it queries
	// anything.
	ServicePrefix = "pnk_"
	// GrantPrefix marks what `panorama login` leaves on a laptop.
	//
	// It is long-lived, and it is deliberately not accepted at the gateway:
	// Parse refuses it, so a grant that leaks cannot be spent on model calls.
	// It is presented to one endpoint, which exchanges it for a short-lived
	// gateway token.
	GrantPrefix = "png_"
)

// gatewayPrefixes are the credentials that may authenticate a model request.
//
// An allowlist, and a short one on purpose. A credential kind is admitted here
// only when it is meant to be spendable; everything else Panorama issues is
// refused at the door rather than looked up and rejected later.
var gatewayPrefixes = []string{Prefix, ServicePrefix}

// userCodeAlphabet is what a device-authorization code is drawn from.
//
// I, L, O, 0 and 1 are absent because this is the one credential in the product
// that is read off one screen and typed into another. U is absent too: it is
// the letter most often produced when someone transcribes a V by hand.
const userCodeAlphabet = "ABCDEFGHJKMNPQRSTVWXYZ23456789"

// userCodeLength is the number of characters in a user code, excluding the
// separating dash. Thirty characters to the alphabet and eight positions is
// a little over 39 bits, which is far more than a code living for ten minutes
// behind a rate limiter needs - and it is not what protects the flow in any
// case. The device code does that; this one only has to be unguessable inside
// its own window.
const userCodeLength = 8

// NewUserCode mints the code a human reads and types, in the form ABCD-EFGH.
//
// Drawn by rejection sampling rather than by reducing a random byte modulo the
// alphabet: 256 is not a multiple of 30, so the modulo would make the first
// sixteen characters likelier than the last fourteen. The skew is small and it
// would never show up in testing, which is exactly why it is worth not writing.
func NewUserCode() (string, error) {
	// The largest multiple of the alphabet that fits in a byte. Draws at or
	// above it are discarded, so every character is equally likely.
	limit := byte(256 / len(userCodeAlphabet) * len(userCodeAlphabet))

	out := make([]byte, 0, userCodeLength+1)
	buf := make([]byte, 1)
	for len(out) < userCodeLength+1 {
		if len(out) == userCodeLength/2 {
			out = append(out, '-')
			continue
		}
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		if buf[0] >= limit {
			continue
		}
		out = append(out, userCodeAlphabet[buf[0]%byte(len(userCodeAlphabet))])
	}
	return string(out), nil
}

// NewSecret mints a random credential with the given prefix.
//
// Everything Panorama issues goes through here, so there is one place where the
// entropy is decided and one place to change it. The returned Hash is what
// persists; the plaintext exists only in the process that made it.
func NewSecret(prefix string) (Token, error) {
	b := make([]byte, secretBytes)
	if _, err := rand.Read(b); err != nil {
		return Token{}, err
	}

	plaintext := prefix + base64.RawURLEncoding.EncodeToString(b)

	display := plaintext
	if len(prefix)+6 < len(plaintext) {
		display = plaintext[:len(prefix)+6]
	}

	return Token{
		Plaintext:     plaintext,
		Hash:          Hash(plaintext),
		DisplayPrefix: display,
	}, nil
}

// NewCSRFToken mints the value a console page must echo back in a header.
//
// It is not a secret from the page - the page reads it out of a cookie and
// sends it - it is a value a cross-origin caller cannot read and therefore
// cannot set. Its only job is to be unguessable.
func NewCSRFToken() (string, error) {
	b := make([]byte, secretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// WellFormed reports whether a value could be a credential this package issued
// with the given prefix.
//
// It is an allowlist check performed before any lookup, so a value that is not
// one of ours is never hashed, never used as a query parameter, and never
// recorded. A stray real Anthropic key pasted into the wrong field stops here.
func WellFormed(prefix, candidate string) bool {
	if !strings.HasPrefix(candidate, prefix) {
		return false
	}
	return len(candidate) == len(prefix)+base64.RawURLEncoding.EncodedLen(secretBytes)
}

// SecretsMatch compares two secrets in constant time.
//
// Used only where a value has to be compared against one already in hand - the
// setup code, and the CSRF token. Token and session lookup does not use it,
// because hashing and looking the hash up directly leaks no timing signal to
// begin with.
func SecretsMatch(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
