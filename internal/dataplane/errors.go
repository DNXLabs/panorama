// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package dataplane

import (
	"encoding/json"
	"net/http"
)

// Anthropic error types. Clients - Claude Code included - branch on these, so
// the value has to be the one the real API would send for the situation.
const (
	errTypeInvalidRequest  = "invalid_request_error"
	errTypeAuthentication  = "authentication_error"
	errTypePermission      = "permission_error"
	errTypeNotFound        = "not_found_error"
	errTypeRateLimit       = "rate_limit_error"
	errTypeAPI             = "api_error"
	errTypeOverloaded      = "overloaded_error"
	errTypeRequestTooLarge = "request_too_large"
)

// apiError is the Anthropic error envelope.
//
// Panorama speaks the client's own error format rather than inventing one, so
// a failure surfaces in the tool's normal error display instead of as an
// unparseable blob.
type apiError struct {
	Type  string       `json:"type"`
	Error apiErrorBody `json:"error"`
}

type apiErrorBody struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	// RequestID is not part of Anthropic's schema. It is additive, ignored by
	// clients that don't know it, and it is what turns "it failed" into
	// something an operator can find in the audit trail.
	RequestID string `json:"request_id,omitempty"`
}

// writeError sends an Anthropic-shaped error.
func writeError(w http.ResponseWriter, status int, errType, message, requestID string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)

	// The status line is already committed; a failed encode can only be
	// logged by the caller's access-log line, not corrected.
	_ = json.NewEncoder(w).Encode(apiError{
		Type: "error",
		Error: apiErrorBody{
			Type:      errType,
			Message:   message,
			RequestID: requestID,
		},
	})
}

// writeStreamError emits an error into an already-started SSE stream.
//
// Once the status line has gone out, the only honest way to report a failure is
// in-band. Anthropic's own streams carry an `error` event for exactly this, so
// clients already handle it.
func writeStreamError(w http.ResponseWriter, errType, message, requestID string) error {
	payload, err := json.Marshal(apiError{
		Type: "error",
		Error: apiErrorBody{
			Type:      errType,
			Message:   message,
			RequestID: requestID,
		},
	})
	if err != nil {
		return err
	}
	return writeSSE(w, "error", payload)
}
