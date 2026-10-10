// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

// Package auth mints and verifies the per-user gateway tokens that coding
// tools present to the gateway.
//
// A token is shown to its owner exactly once, at creation. Only its SHA-256
// hash is stored, so a database disclosure hands over no working credentials.
// Verification hashes the presented token and looks the hash up directly -
// there is no comparison against a stored secret, and therefore no timing
// signal to leak.
package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

// Prefix marks a Panorama gateway token. It makes a leaked token
// recognisable in a log or a paste, which is what secret scanners key on.
const Prefix = "pnt_"

// secretBytes is the entropy behind each token.
const secretBytes = 32

// GatewayTokenTTL is how long a minted gateway token lives, and it is not a
// default anybody may raise.
//
// A pasted token is the credential this product is least able to protect: it
// is written into a shell profile or a settings file, it does not rotate, and
// nothing about a laptop that leaves the building tells the deployment to
// revoke it. An expiry is the one guarantee available without asking anybody
// to remember something - thirty days is long enough that nobody is minting
// one weekly, and short enough that a token forgotten in a dotfile stops
// working inside a quarter.
//
// The recommended path does not depend on it: `panorama login` leaves a grant,
// and the tokens that grant exchanges for live for auth.token_ttl (an hour by
// default) and rotate on their own. This is the ceiling on the path that does
// not.
const GatewayTokenTTL = 30 * 24 * time.Hour

// GatewayTokenExpiry is when a token minted now stops working.
//
// One function for every mint path - the console and `panorama token create` -
// because two places deciding a credential's lifetime is two answers to "when
// does this stop working", and the day they disagreed the longer one would
// win silently.
//
// A deployment's own auth.max_token_ttl still binds, and binds downward only:
// an operator who has said tokens last a week has said something stricter than
// this, and clamping is the right reading of that. Raising it is not on offer,
// which is what "always" means here.
func GatewayTokenExpiry(now time.Time, maxTTL time.Duration) time.Time {
	return now.UTC().Add(GatewayTokenLifetime(maxTTL))
}

// GatewayTokenLifetime is how long a token minted now would last, which is
// what a screen has to say before anybody mints one.
func GatewayTokenLifetime(maxTTL time.Duration) time.Duration {
	if maxTTL > 0 && maxTTL < GatewayTokenTTL {
		return maxTTL
	}
	return GatewayTokenTTL
}

// ErrMalformedToken is returned for a value that cannot be a gateway token.
var ErrMalformedToken = errors.New("auth: malformed gateway token")

// Token is a freshly minted credential. Plaintext exists only in the process
// that created it and is never stored.
type Token struct {
	// Plaintext is shown to the user once and then discarded.
	Plaintext string
	// Hash is the SHA-256 of Plaintext, hex encoded. This is what persists.
	Hash string
	// DisplayPrefix is the leading characters of Plaintext, safe to store and
	// show so a user can identify a token in a list.
	DisplayPrefix string
}

// Generate mints a new per-user gateway token.
func Generate() (Token, error) { return NewSecret(Prefix) }

// GenerateService mints a new team-owned service token.
func GenerateService() (Token, error) { return NewSecret(ServicePrefix) }

// GenerateGrant mints what `panorama login` leaves on a laptop.
func GenerateGrant() (Token, error) { return NewSecret(GrantPrefix) }

// KindOf reports which credential a well-formed gateway token is, so the
// caller can look in exactly one table.
//
// This is why the prefixes differ. Authentication on the request path stays a
// single indexed lookup: the kind is decided from four characters, before any
// query is built, rather than by trying one table and then another.
func KindOf(token string) string {
	switch {
	case strings.HasPrefix(token, ServicePrefix):
		return KindService
	case strings.HasPrefix(token, Prefix):
		return KindUser
	default:
		return ""
	}
}

// The credential kinds that may authenticate a model request.
const (
	KindUser    = "user"
	KindService = "service"
)

// Hash returns the stored form of a token.
func Hash(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// Parse extracts a gateway token from a request's credentials, accepting both
// forms a coding tool might send.
//
// Claude Code sends `Authorization: Bearer <token>` when configured with
// ANTHROPIC_AUTH_TOKEN, and `x-api-key: <token>` when configured with
// ANTHROPIC_API_KEY. Both are supported so either environment variable works.
//
// It accepts a personal token and a team service token, and nothing else. A
// CLI grant is refused here even though it is a valid Panorama credential:
// a grant is long-lived, and the only thing it may be exchanged for is the
// short-lived token this function does accept.
//
// This is an allowlist: a value that is not shaped like a Panorama gateway
// token is rejected here rather than being hashed and looked up, so a stray
// real Anthropic key is never sent anywhere or recorded.
func Parse(authorization, apiKey string) (string, error) {
	candidate := apiKey

	if authorization != "" {
		scheme, rest, ok := strings.Cut(authorization, " ")
		if !ok || !strings.EqualFold(scheme, "bearer") {
			return "", ErrMalformedToken
		}
		candidate = strings.TrimSpace(rest)
	}

	if candidate == "" {
		return "", ErrMalformedToken
	}
	for _, prefix := range gatewayPrefixes {
		if WellFormed(prefix, candidate) {
			return candidate, nil
		}
	}
	return "", ErrMalformedToken
}

// ParseGrant extracts a CLI grant from an Authorization header.
//
// Separate from Parse, and deliberately not reachable from it. The grant is
// presented to exactly one endpoint - the token exchange - and keeping the two
// parsers apart means a handler cannot accidentally accept the wrong one by
// calling the more convenient function.
func ParseGrant(authorization string) (string, error) {
	scheme, rest, ok := strings.Cut(authorization, " ")
	if !ok || !strings.EqualFold(scheme, "bearer") {
		return "", ErrMalformedToken
	}
	candidate := strings.TrimSpace(rest)
	if !WellFormed(GrantPrefix, candidate) {
		return "", ErrMalformedToken
	}
	return candidate, nil
}
