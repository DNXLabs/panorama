// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"
)

// Middleware wraps a handler.
type Middleware func(http.Handler) http.Handler

// chain applies middleware so that the first argument after the handler is the
// outermost wrapper - the order they are listed is the order a request passes
// through them.
func chain(h http.Handler, mw ...Middleware) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}

type contextKey int

const requestIDKey contextKey = iota

// RequestIDFrom returns the request identifier assigned to this request, or ""
// outside a request.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// requestID assigns every request an identifier and echoes it back, so an
// engineer hitting an error can quote a value that finds the exact request in
// the logs.
//
// The identifier is always generated here. An inbound X-Request-Id is not
// honoured: a client-supplied value crosses a trust boundary, and accepting it
// would let a caller collide or forge log correlation.
func requestID() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := newRequestID()
			w.Header().Set("X-Request-Id", id)

			ctx := context.WithValue(r.Context(), requestIDKey, id)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func newRequestID() string {
	var b [16]byte
	// rand.Read from crypto/rand cannot fail; since Go 1.24 it panics rather
	// than returning an error.
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// accessLog records one line per request.
//
// The fields here are what the control plane knows about any request. Identity
// and routing context - userId, teamId, sessionId, model, provider, tool - are
// attached by the data plane, which is where they are known.
func accessLog(log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &recorder{ResponseWriter: w, status: http.StatusOK}

			next.ServeHTTP(rec, r)

			level := slog.LevelInfo
			switch {
			case rec.status >= http.StatusInternalServerError:
				level = slog.LevelError
			case rec.status >= http.StatusBadRequest:
				level = slog.LevelWarn
			case r.URL.Path == "/healthz" || r.URL.Path == "/readyz":
				// Probes fire every few seconds; logging each one at info
				// buries everything else.
				level = slog.LevelDebug
			}

			log.LogAttrs(r.Context(), level, "request",
				slog.String("request_id", RequestIDFrom(r.Context())),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.status),
				slog.Int64("bytes", rec.written),
				slog.Duration("duration", time.Since(start)),
				slog.String("remote_addr", r.RemoteAddr),
			)
		})
	}
}

// recoverer turns a panic into a 500 and a logged stack rather than a dropped
// connection.
func recoverer(log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// This closure runs during unwinding, after the handler has
			// returned. r.Context() is the request's own context and is the
			// right one to log against, which is what the suppression below
			// asserts.
			defer func() { //nolint:contextcheck // the deferred recover legitimately uses the request context
				rv := recover()
				if rv == nil {
					return
				}
				// ErrAbortHandler is the standard library's way of saying "this
				// connection is being torn down deliberately". Re-panic so
				// net/http handles it as intended.
				if rv == http.ErrAbortHandler { //nolint:errorlint // sentinel compared by identity, as net/http does
					panic(rv)
				}

				log.LogAttrs(r.Context(), slog.LevelError, "panic serving request",
					slog.String("request_id", RequestIDFrom(r.Context())),
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.Any("panic", rv),
					slog.String("stack", string(debug.Stack())),
				)

				// If the handler already started writing - a stream, most
				// likely - the status is committed and anything further would
				// corrupt the body. The log line above is the whole record.
				//
				// This depends on recoverer being wrapped by accessLog, which
				// is what installs the recorder. See the chain in New.
				if c, ok := w.(committer); ok && c.Committed() {
					return
				}
				writeJSON(w, http.StatusInternalServerError, map[string]string{
					"error":      "internal error",
					"request_id": RequestIDFrom(r.Context()),
				})
			}()

			next.ServeHTTP(w, r)
		})
	}
}

// committer reports whether a response has been started. The recoverer uses it
// rather than a concrete type so any future wrapper can answer the same
// question.
type committer interface {
	Committed() bool
}

// recorder captures the status and byte count for the access log.
//
// It implements Unwrap, which is what http.ResponseController uses to reach the
// underlying writer. Without it, wrapping the ResponseWriter would silently
// disable flushing and hijacking - and a proxy that cannot flush cannot stream.
type recorder struct {
	http.ResponseWriter

	status      int
	written     int64
	wroteHeader bool
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Committed reports whether a status line has gone out, after which the
// response can only be appended to, never replaced.
func (r *recorder) Committed() bool { return r.wroteHeader }

func (r *recorder) WriteHeader(code int) {
	if r.wroteHeader {
		return
	}
	r.status = code
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(b)
	r.written += int64(n)
	return n, err
}
