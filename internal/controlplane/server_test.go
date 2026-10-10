// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dnxlabs/panorama/internal/config"
	"github.com/dnxlabs/panorama/internal/store"
)

func TestHealthIsUpWithoutTouchingDependencies(t *testing.T) {
	s := newTestServer(t, false)

	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var body map[string]string
	decodeJSON(t, rec.Body.Bytes(), &body)
	if body["status"] != "ok" {
		t.Errorf("status = %q, want ok", body["status"])
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

func TestReadyIsNotReadyWhileMigrationsArePending(t *testing.T) {
	s := newTestServer(t, false)

	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 while the schema is behind", rec.Code)
	}

	var body readyResponse
	decodeJSON(t, rec.Body.Bytes(), &body)
	if body.Status != "not ready" {
		t.Errorf("status = %q, want \"not ready\"", body.Status)
	}
	if !strings.Contains(body.Checks["schema"], "pending") {
		t.Errorf("schema check = %q, want it to name the pending migrations", body.Checks["schema"])
	}
	if body.Checks["database"] != "ok" {
		t.Errorf("database check = %q, want ok - the database is reachable", body.Checks["database"])
	}
}

func TestReadyIsReadyOnACurrentSchema(t *testing.T) {
	s := newTestServer(t, true)

	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body)
	}

	var body readyResponse
	decodeJSON(t, rec.Body.Bytes(), &body)
	if body.Status != "ready" {
		t.Errorf("status = %q, want ready", body.Status)
	}
	for _, check := range []string{"database", "schema"} {
		if body.Checks[check] != "ok" {
			t.Errorf("%s check = %q, want ok", check, body.Checks[check])
		}
	}
}

func TestReadyIsNotReadyOnAClosedDatabase(t *testing.T) {
	s := newTestServer(t, true)
	if err := s.db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 with the database gone", rec.Code)
	}

	var body readyResponse
	decodeJSON(t, rec.Body.Bytes(), &body)
	if body.Checks["database"] != "unreachable" {
		t.Errorf("database check = %q, want unreachable", body.Checks["database"])
	}
}

func TestHealthEndpointsAreTheOnlyRoutes(t *testing.T) {
	s := newTestServer(t, true)

	// Everything else is a 404 until something registers it. This is the
	// baseline the "authenticate every endpoint" rule is measured against.
	for _, path := range []string{"/", "/v1/messages", "/api/users", "/debug/pprof/"} {
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404 - no route should exist yet", path, rec.Code)
		}
	}
}

func TestRequestIDIsGeneratedAndNotTakenFromTheClient(t *testing.T) {
	handler := chain(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if RequestIDFrom(r.Context()) == "" {
				t.Error("no request id in the handler's context")
			}
			w.WriteHeader(http.StatusNoContent)
		}),
		requestID(),
	)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-Id", "client-supplied-forgery")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	got := rec.Header().Get("X-Request-Id")
	if got == "" {
		t.Fatal("no X-Request-Id on the response")
	}
	if got == "client-supplied-forgery" {
		t.Error("the client-supplied request id was honoured; it crosses a trust boundary and must be replaced")
	}
	if len(got) != 32 {
		t.Errorf("X-Request-Id = %q, want 32 hex characters", got)
	}
}

func TestRequestIDsAreUnique(t *testing.T) {
	seen := make(map[string]bool, 100)
	for range 100 {
		id := newRequestID()
		if seen[id] {
			t.Fatalf("duplicate request id %q", id)
		}
		seen[id] = true
	}
}

// TestRecovererTurnsAPanicIntoA500 exercises the real server's middleware
// chain rather than a hand-assembled one, because the ordering of that chain is
// what makes the request id and the committed-response check work at all.
func TestRecovererTurnsAPanicIntoA500(t *testing.T) {
	s := newTestServer(t, true)
	s.Handle("GET /panic", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))

	rec := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panic", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}

	var body map[string]string
	decodeJSON(t, rec.Body.Bytes(), &body)
	if body["error"] != "internal error" {
		t.Errorf("error = %q, want a generic message that leaks nothing", body["error"])
	}
	if body["request_id"] == "" {
		t.Error("the 500 should carry the request id so the user can quote it")
	}
	if strings.Contains(rec.Body.String(), "boom") {
		t.Error("the panic value leaked into the response body")
	}
}

func TestRecovererDoesNotRewriteACommittedResponse(t *testing.T) {
	// A stream that panics halfway has already sent its status and part of its
	// body. Writing a 500 over the top would corrupt what the client is
	// parsing.
	s := newTestServer(t, true)
	s.Handle("GET /panic-mid-stream", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		if _, err := io.WriteString(w, "event: message\n"); err != nil {
			t.Errorf("write: %v", err)
		}
		panic("boom mid-stream")
	}))

	rec := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panic-mid-stream", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want the already-committed 200", rec.Code)
	}
	if got := rec.Body.String(); got != "event: message\n" {
		t.Errorf("body = %q, want the partial stream left untouched", got)
	}
}

// TestRecorderPreservesFlushing is the test that matters most in this file.
// The access log wraps every ResponseWriter, and a wrapper that does not
// implement Unwrap silently disables flushing - which would make streaming
// impossible without any visible error.
func TestRecorderPreservesFlushing(t *testing.T) {
	var flushed bool

	s := newTestServer(t, true)
	s.Handle("GET /stream", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		rc := http.NewResponseController(w)
		if _, err := io.WriteString(w, "chunk"); err != nil {
			t.Errorf("write: %v", err)
		}
		if err := rc.Flush(); err != nil {
			t.Errorf("Flush through the middleware chain failed: %v - "+
				"streaming would be broken", err)
			return
		}
		flushed = true
	}))

	s.http.Handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/stream", nil))

	if !flushed {
		t.Error("the handler could not flush; nothing may buffer a stream")
	}
}

// TestServerChainSetsRequestID checks the assembled chain, not the middleware
// in isolation: an ordering mistake here is invisible until someone tries to
// correlate a production error with a log line.
func TestServerChainSetsRequestID(t *testing.T) {
	s := newTestServer(t, true)

	rec := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Header().Get("X-Request-Id") == "" {
		t.Error("no X-Request-Id on a response served through the real chain")
	}
}

func TestRecorderCountsBytesAndStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	r := &recorder{ResponseWriter: rec, status: http.StatusOK}

	if _, err := io.WriteString(r, "hello"); err != nil {
		t.Fatalf("write: %v", err)
	}
	// A second WriteHeader after the implicit 200 must not change the record.
	r.WriteHeader(http.StatusTeapot)

	if r.status != http.StatusOK {
		t.Errorf("status = %d, want the first status written (200)", r.status)
	}
	if r.written != 5 {
		t.Errorf("written = %d, want 5", r.written)
	}
}

func newTestServer(t *testing.T, migrate bool) *Server {
	t.Helper()

	cfg := config.Default()
	cfg.Database.DSN = "file:" + filepath.Join(t.TempDir(), "panorama.db")

	db, err := store.Open(t.Context(), cfg.Database)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		// Tests that close the handle themselves make this a no-op error.
		_ = db.Close()
	})

	if migrate {
		if err := db.Migrate(t.Context()); err != nil {
			t.Fatalf("migrate: %v", err)
		}
	}

	return New(cfg, db, discardLogger())
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func decodeJSON(t *testing.T, b []byte, into any) {
	t.Helper()
	if err := json.Unmarshal(b, into); err != nil {
		t.Fatalf("decode response %s: %v", b, err)
	}
}

// The development sign-in route is registered rather than guarded, so what
// there is to test is its absence: on every deployment that did not ask for it,
// the path must be a 404 from the mux - no handler to reach, nothing to probe
// for, and no check that could be wrong.
func TestDevSignInRouteExistsOnlyWhereItIsConfigured(t *testing.T) {
	t.Run("absent by default", func(t *testing.T) {
		s := newTestServer(t, true)

		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/auth/dev", nil))

		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404 - the route must not exist unless dev.sign_in_as is set", rec.Code)
		}
	})

	t.Run("not announced by default", func(t *testing.T) {
		s := newTestServer(t, true)

		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/auth/state", nil))

		var state struct {
			DevSignIn bool `json:"dev_sign_in"`
		}
		decodeJSON(t, rec.Body.Bytes(), &state)
		if state.DevSignIn {
			t.Error("the sign-in gate must not offer a door this deployment does not have")
		}
	})

	t.Run("opens an administrator session when configured", func(t *testing.T) {
		s := newDevSignInServer(t)

		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/auth/dev", nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
		}

		// Through UserByID rather than UserByEmail: the email lookup does not
		// select role_source, and the pinning is half of what is being checked.
		found, err := s.db.UserByEmail(t.Context(), "dev@localhost")
		if err != nil {
			t.Fatalf("the account should have been created: %v", err)
		}
		user, err := s.db.UserByID(t.Context(), found.ID)
		if err != nil {
			t.Fatalf("reading the account back: %v", err)
		}
		if user.Role != store.RoleAdmin {
			t.Errorf("role = %q, want %q - the whole point is seeing every screen", user.Role, store.RoleAdmin)
		}
		// Pinned, so a second sign-in does not recompute it from a group
		// mapping the account is in none of and demote it to the default role.
		if user.RoleSource != store.RoleSourceManual {
			t.Errorf("role source = %q, want %q", user.RoleSource, store.RoleSourceManual)
		}
	})

	t.Run("signing in twice keeps the same account and role", func(t *testing.T) {
		s := newDevSignInServer(t)

		for i := range 2 {
			rec := httptest.NewRecorder()
			s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/auth/dev", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("sign-in %d: status = %d, want 200: %s", i+1, rec.Code, rec.Body)
			}
		}

		user, err := s.db.UserByEmail(t.Context(), "dev@localhost")
		if err != nil {
			t.Fatalf("reading the account: %v", err)
		}
		if user.Role != store.RoleAdmin {
			t.Errorf("role = %q after a second sign-in, want %q", user.Role, store.RoleAdmin)
		}
	})
}

// newDevSignInServer is a deployment with the development sign-in on, and a
// base URL that config validation would accept for it.
func newDevSignInServer(t *testing.T) *Server {
	t.Helper()

	cfg := config.Default()
	cfg.Database.DSN = "file:" + filepath.Join(t.TempDir(), "panorama.db")
	cfg.Server.BaseURL = "http://localhost:5173"
	cfg.Dev.SignInAs = "dev@localhost"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("this configuration must be one Panorama would start with: %v", err)
	}

	db, err := store.Open(t.Context(), cfg.Database)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(t.Context()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	return New(cfg, db, discardLogger())
}
