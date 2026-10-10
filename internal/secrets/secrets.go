// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

// Package secrets encrypts the credentials Panorama has to be able to present.
//
// # Why this is not hashing
//
// Every credential Panorama *issues* is stored as a SHA-256 and never in the
// clear, because verifying one only requires hashing what was presented and
// comparing. Nothing has to recover the original.
//
// A credential Panorama *holds* is the opposite. A GitHub App private key has
// to be signed with, and a personal access token has to be sent - so it must
// come back out, and a hash cannot do that. What is available instead is
// encryption, which moves the problem to where the key is kept.
//
// # Where the key is kept, and why that is the whole point
//
// The key lives in the configuration or the environment; the ciphertext lives
// in the database. That split is deliberate and is what makes storing a
// credential in a database defensible at all: a database dump, a replica, a
// nightly backup, or a stolen volume contains no working credential without a
// key that was never in any of them.
//
// It also means the honest failure mode is stated rather than hidden - lose the
// key and the stored credentials are gone, which is a re-entry of two fields in
// the console rather than a recovery problem.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"unicode/utf8"
)

// KeyBytes is the key length, in bytes. AES-256.
const KeyBytes = 32

// MinPassphrase is the shortest passphrase a key may be derived from.
//
// Any string is accepted as an encryption key, but what it protects is only as
// strong as the string: the derivation is public and unsalted by necessity -
// there is nowhere to keep a salt that the key itself is not already kept - so
// somebody holding a database dump can try candidates offline as fast as they
// can hash. The floor is what keeps "a dump carries nothing usable" true of a
// key somebody typed rather than minted. It is deliberately low enough that a
// passphrase anyone would actually choose clears it.
const MinPassphrase = 16

// derivationInfo separates this key from any other thing the same passphrase
// might be used to derive.
//
// It is an input to the derivation, so changing this string changes every
// derived key and orphans every credential sealed under one. It does not
// change. A second version would be a new constant and a migration, not an
// edit to this one.
//
// The value is bytes, not a name: it is fixed forever because it is part of
// the key derivation.
const derivationInfo = "panorama encryption key v1"

// ErrNoKey is returned when something needs to encrypt or decrypt and no key is
// configured.
//
// A distinct error rather than a generic failure, because the console turns it
// into the one sentence that resolves it: set PANORAMA_ENCRYPTION_KEY. A
// deployment that has not set one cannot store a credential, which is the safe
// default - it fails at the moment somebody tries, with the reason, instead of
// writing a secret somewhere it should not go.
var ErrNoKey = errors.New("secrets: no encryption key is configured")

// ErrWrongKey is returned when a value cannot be decrypted with the key in
// hand.
//
// Almost always means the key changed - rotated, regenerated, or a different
// deployment's key pointed at the same database. Said as its own error so the
// console can offer the action that actually fixes it, which is re-entering the
// credential rather than debugging.
var ErrWrongKey = errors.New("secrets: the stored value cannot be read with this key")

// Box seals and opens values with one key.
//
// The zero Box has no key and refuses everything with ErrNoKey, which is what a
// deployment that has not configured one gets. That is a usable state:
// everything except storing a credential still works.
type Box struct {
	aead cipher.AEAD
	// derived records that the key was computed from a passphrase rather than
	// used as given, so startup can say so. See DeriveKey.
	derived bool
}

// NewBox builds a box from a configured key - either 32 base64 bytes or a
// passphrase to derive them from; see DeriveKey. An empty key yields a keyless
// box rather than an error, because running without one is a supported
// deployment.
func NewBox(configuredKey string) (*Box, error) {
	if configuredKey == "" {
		return &Box{}, nil
	}

	key, derived, err := DeriveKey(configuredKey)
	if err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secrets: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secrets: %w", err)
	}
	return &Box{aead: aead, derived: derived}, nil
}

// Configured reports whether this box can store anything.
func (b *Box) Configured() bool { return b != nil && b.aead != nil }

// Derived reports whether the key was derived from a passphrase rather than
// taken as given. Startup logs it: a deployment whose 32-byte key was mangled
// into a passphrase still runs, and this is the line that says so.
func (b *Box) Derived() bool { return b != nil && b.derived }

// Seal encrypts a value for storage.
//
// The nonce is random per call and prepended to the ciphertext, so the same
// credential stored twice produces two different rows - which is what stops the
// database revealing that two deployments share a token.
func (b *Box) Seal(plaintext string) (string, error) {
	if !b.Configured() {
		return "", ErrNoKey
	}

	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("secrets: %w", err)
	}

	sealed := b.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Open decrypts a stored value.
func (b *Box) Open(encoded string) (string, error) {
	if !b.Configured() {
		return "", ErrNoKey
	}

	sealed, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", ErrWrongKey
	}
	if len(sealed) < b.aead.NonceSize() {
		return "", ErrWrongKey
	}

	nonce, body := sealed[:b.aead.NonceSize()], sealed[b.aead.NonceSize():]
	plaintext, err := b.aead.Open(nil, nonce, body, nil)
	if err != nil {
		// GCM authenticates, so this is a wrong key or a tampered value and
		// there is no way to tell which. Both need the same answer.
		return "", ErrWrongKey
	}
	return string(plaintext), nil
}

// NewKey mints a key, base64 encoded, for `panorama admin generate-key`. Its
// output takes DeriveKey's first branch - it is used as minted, never derived
// from.
func NewKey() (string, error) {
	key := make([]byte, KeyBytes)
	if _, err := rand.Read(key); err != nil {
		return "", fmt.Errorf("secrets: %w", err)
	}
	return base64.StdEncoding.EncodeToString(key), nil
}

// DeriveKey turns whatever is configured into the 32 bytes AES needs, and
// reports whether it had to derive them.
//
// Two inputs are accepted and the distinction between them is load-bearing:
//
//   - Base64 that decodes to exactly KeyBytes is the key, used exactly as
//     given. Every key `panorama admin generate-key` mints has this shape, so
//     deriving from one instead of decoding it would silently change the key
//     on a deployment that has credentials sealed under it, and turn every one
//     of them into ErrWrongKey. This branch must stay first.
//
//   - Anything else is a passphrase, and the key is derived from it with
//     HKDF-SHA256: extract to a fixed-width digest, expand to KeyBytes. The
//     expand step is what makes the length KeyBytes' to choose rather than
//     SHA-256's to dictate.
//
// The cost of accepting both is that a 32-byte key somebody truncates by
// accident does not fail: it derives a different, working key and the
// credentials sealed under the original stop opening. That is why the second
// return value exists: startup says which branch it took, so the surprising
// case is in the log rather than only in a support ticket.
//
// Validated at startup rather than at the first save, so a key nobody can use
// is a boot error naming the key instead of a cipher error naming nothing.
func DeriveKey(configured string) (key []byte, derived bool, err error) {
	if raw, decodeErr := base64.StdEncoding.DecodeString(configured); decodeErr == nil && len(raw) == KeyBytes {
		return raw, false, nil
	}

	if n := utf8.RuneCountInString(configured); n < MinPassphrase {
		return nil, false, fmt.Errorf(
			"secrets: the encryption key is %d characters, want at least %d - any unguessable phrase works, or mint one with `panorama admin generate-key`",
			n, MinPassphrase)
	}

	raw, err := hkdf.Key(sha256.New, []byte(configured), nil, derivationInfo, KeyBytes)
	if err != nil {
		return nil, false, fmt.Errorf("secrets: %w", err)
	}
	return raw, true, nil
}
