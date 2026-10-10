// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"crypto/rand"
	"encoding/base32"
	"strings"
)

// idAlphabet is Crockford-style base32 without padding: case-insensitive,
// URL-safe, and free of the characters that get misread when someone copies an
// identifier out of a log line.
var idAlphabet = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

// NewID returns a random identifier for a stored row.
//
// Row identifiers are random rather than sequential on purpose: they appear in
// URLs and API responses, and a sequential id leaks how much traffic a
// deployment handles.
func NewID(prefix string) string {
	var b [16]byte
	// crypto/rand.Read cannot fail; since Go 1.24 it panics rather than
	// returning an error.
	rand.Read(b[:])

	var sb strings.Builder
	sb.Grow(len(prefix) + 1 + idAlphabet.EncodedLen(len(b)))
	if prefix != "" {
		sb.WriteString(prefix)
		sb.WriteByte('_')
	}
	sb.WriteString(idAlphabet.EncodeToString(b[:]))
	return sb.String()
}
