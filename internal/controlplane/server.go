// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

// Package controlplane serves Panorama's HTTP surface: operational endpoints,
// the console REST API, OIDC sign-in, and token minting. The data plane mounts its proxy routes onto the same mux, so
// one deployment listens on one port.
package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/dnxlabs/panorama/internal/config"
	"github.com/dnxlabs/panorama/internal/store"
	"github.com/dnxlabs/panorama/internal/version"
)

// Server owns the HTTP listener and the route table.
type Server struct {
	cfg  config.Config
	db   *store.DB
	log  *slog.Logger
	mux  *http.ServeMux
	http *http.Server

	identity *identity
	// setupLimiter bounds attempts on the one endpoint that creates an
	// account without a credential behind it.
	setupLimiter *attemptLimiter
}

// New builds a server with the operational routes registered. Feature routes
// are added through Handle.
func New(cfg config.Config, db *store.DB, log *slog.Logger) *Server {
	mux := http.NewServeMux()

	s := &Server{
		cfg:          cfg,
		db:           db,
		log:          log,
		mux:          mux,
		identity:     newIdentity(cfg),
		setupLimiter: newAttemptLimiter(10, time.Minute),
	}

	s.http = &http.Server{
		Addr: cfg.Server.Addr,
		// Order is load-bearing, outermost first:
		//   requestID  assigns the id, so everything below can log it.
		//   accessLog  installs the recorder and logs the final status, so it
		//              must sit outside recoverer to record the 500 a panic
		//              produces.
		//   recoverer  needs the recorder as its ResponseWriter to tell a
		//              committed stream from a response it may still replace.
		Handler: chain(mux, requestID(), accessLog(log), recoverer(log)),
		// Header timeouts only. ReadTimeout and WriteTimeout are deliberately
		// unset: a streaming response is long-lived by design, and a write
		// deadline would sever it mid-stream.
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	s.routes()
	return s
}

// Handle registers a handler, so the data plane and the console API can mount
// onto this server without this package importing them.
func (s *Server) Handle(pattern string, h http.Handler) {
	s.mux.Handle(pattern, h)
}

// routes registers the unauthenticated surface.
//
// This is the whole of it, and every entry needs a reason it cannot be
// authenticated rather than a reason it need not be:
//
//   - the probes, because a load balancer has no credential and they expose no
//     organizational data;
//   - the sign-in state, because a visitor has to be told whether to offer an
//     SSO button or a setup code before they can have a credential;
//   - redeeming a setup code, because the person holding one has by
//     construction no way to sign in yet. It is protected by a 256-bit code
//     that exists only in this deployment's log or an operator's terminal;
//   - the two OIDC endpoints, because they are the thing that produces
//     authentication. Both are bound to the browser that started the flow;
//   - signing in with a gateway token, while single sign-on is not configured.
//     It is not unauthenticated so much as authenticated by a credential
//     the browser cannot otherwise present: the token already carries its
//     owner's full /api/v1 rights, so the session it opens grants nothing
//     the token did not. It is not registered once an issuer is, so an
//     organization's identity provider is then the only way into a browser.
//     See token_sign_in.go;
//   - the development sign-in, which is the exception that proves the rule: it
//     authenticates nobody, and the only reason it is defensible is that it is
//     not registered at all unless an operator asked for it AND this deployment
//     is reachable only from the machine it runs on - a condition config
//     validation refuses to start without.
//
// Adding anything else here is a review conversation, not a commit.
func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	s.mux.HandleFunc("GET /readyz", s.handleReady)

	s.mux.HandleFunc("GET /api/v1/auth/state", s.handleAuthState)
	s.mux.HandleFunc("POST /api/v1/auth/setup", s.handleSetupSession)

	s.mux.HandleFunc("GET /auth/oidc/start", s.handleOIDCStart)
	s.mux.HandleFunc("GET /auth/oidc/callback", s.handleOIDCCallback)

	// The device-authorization flow, which is unauthenticated because the
	// caller having no credential is the problem it exists to solve. Neither
	// endpoint grants anything: the first hands back a code that is inert until
	// a signed-in human approves it on an authenticated, CSRF-protected route,
	// and the second only ever returns what that approval already decided.
	// Neither names an account, discloses whether one exists, or says anything
	// about the deployment.
	//
	// All three are behind requireCLISignIn, which this release answers no to.
	// Guarded rather than left unregistered, unlike the development sign-in:
	// the switch is a database row an administrator throws while the process is
	// running, so a decision made at startup would be the wrong one an hour
	// later. See cli_auth.go.
	s.mux.Handle("POST /api/v1/cli/device", s.requireCLISignIn(http.HandlerFunc(s.handleDeviceStart)))
	s.mux.Handle("POST /api/v1/cli/device/grant", s.requireCLISignIn(http.HandlerFunc(s.handleDeviceGrant)))

	// The token exchange authenticates with a grant, which auth.Parse refuses
	// at the gateway - so this is the only door it opens, and it is not an
	// unauthenticated one.
	s.mux.Handle("POST /api/v1/cli/token", s.requireCLISignIn(http.HandlerFunc(s.handleCLIToken)))

	// Signing in with a gateway token, for a deployment with no identity
	// provider. Decided at startup, like the development sign-in below,
	// because the OIDC connection is configuration read once: with an issuer
	// set the path is a 404 from the mux, not a handler that refuses.
	if !s.cfg.OIDC.Configured() {
		s.mux.HandleFunc("POST /api/v1/auth/token", s.handleTokenSignIn)
	}

	// The development sign-in, the other route in Panorama that is
	// registered conditionally.
	//
	// Registered rather than guarded on purpose: everywhere else this is not
	// configured - which is everywhere that is not a laptop - the path is a 404
	// from the mux, so there is no handler to reach, nothing to probe for, and
	// no check that could be wrong. Config validation has already refused to
	// start if it is set on a deployment anyone else can reach. See dev_auth.go.
	if s.cfg.Dev.Enabled() {
		s.log.Warn("DEVELOPMENT SIGN-IN IS ENABLED - POST /api/v1/auth/dev opens an administrator console session with no identity provider",
			"email", s.cfg.Dev.SignInAs, "base_url", s.cfg.Server.BaseURL,
			"note", "this is for a development stack only; unset PANORAMA_DEV_SIGN_IN_AS anywhere else")
		s.mux.HandleFunc("POST /api/v1/auth/dev", s.handleDevSignIn)
	}
}

// fail records the cause server-side and tells the client only what it can act
// on: a request id to quote.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, what string, err error) {
	s.log.ErrorContext(r.Context(), what,
		"error", err,
		"request_id", RequestIDFrom(r.Context()),
		"path", r.URL.Path,
	)
	writeAPIError(w, r, http.StatusInternalServerError,
		"Panorama could not complete that request. Quote the request id if you report it.")
}

// decodeBody reads a request body into dst, answering the client itself if it
// cannot.
//
// The body is bounded. These are administrative endpoints rather than the
// proxy, and nothing an administrator sends is large; an unbounded decode on an
// authenticated endpoint is still a way to make a process allocate.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	const maxBody = 1 << 20 // 1 MiB

	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	// A field the server does not know is a client asking for something this
	// version cannot do, and accepting it silently is how a console and a
	// gateway drift apart across an upgrade.
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		writeAPIError(w, r, http.StatusBadRequest, "That request body is not valid: "+err.Error())
		return false
	}
	return true
}

// renderPrincipal is the one shape an identity crosses the wire in.
func renderPrincipal(p *store.Principal) map[string]any {
	return map[string]any{
		"id":           p.UserID,
		"email":        p.Email,
		"display_name": p.DisplayName,
		"role":         p.Role,
		// The console renders a different, much smaller application for a setup
		// session. Sent as a field rather than inferred from the empty id,
		// because an account pre-created before its owner's first sign-in has no
		// external id either.
		"kind": kindOrUser(p.Kind),
		// What this role may look at, so the console does not have to encode
		// the rule a second time and get it subtly different.
		"can_see_everyone": seesEveryone(p.Role),
		"is_admin":         p.Role == store.RoleAdmin,
	}
}

// kindOrUser defaults an unset kind, so every principal that resolves to a
// person reads the same whether or not the field was populated.
func kindOrUser(kind string) string {
	if kind == "" {
		return store.PrincipalKindUser
	}
	return kind
}

// handleHealth reports that the process is up and serving. It touches no
// dependencies: a liveness probe that fails when the database is briefly
// unreachable would restart a process that was about to recover.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type readyResponse struct {
	Status  string            `json:"status"`
	Checks  map[string]string `json:"checks"`
	Version string            `json:"version"`
}

// handleReady reports whether this instance can actually serve traffic: the
// database is reachable and the schema is current.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	resp := readyResponse{
		Status:  "ready",
		Checks:  map[string]string{},
		Version: version.Get().Version,
	}
	code := http.StatusOK

	if err := s.db.PingContext(ctx); err != nil {
		s.log.ErrorContext(ctx, "readiness: database unreachable", "error", err)
		resp.Status = "not ready"
		resp.Checks["database"] = "unreachable"
		// Return early: a schema check against a dead connection adds nothing
		// but a second error.
		writeJSON(w, http.StatusServiceUnavailable, resp)
		return
	}
	resp.Checks["database"] = "ok"

	pending, err := s.db.PendingMigrations(ctx)
	switch {
	case err != nil:
		s.log.ErrorContext(ctx, "readiness: cannot read migration status", "error", err)
		resp.Status = "not ready"
		resp.Checks["schema"] = "unknown"
		code = http.StatusServiceUnavailable
	case len(pending) > 0:
		s.log.WarnContext(ctx, "readiness: schema is behind", "pending", len(pending))
		resp.Status = "not ready"
		resp.Checks["schema"] = fmt.Sprintf("%d migrations pending", len(pending))
		code = http.StatusServiceUnavailable
	default:
		resp.Checks["schema"] = "ok"
	}

	writeJSON(w, code, resp)
}

// ListenAndServe serves until ctx is cancelled, then shuts down gracefully,
// giving in-flight requests - including active streams - up to the configured
// shutdown timeout to finish.
func (s *Server) ListenAndServe(ctx context.Context) error {
	errs := make(chan error, 1)

	go func() {
		s.log.Info("listening",
			"addr", s.cfg.Server.Addr,
			"base_url", s.cfg.Server.BaseURL,
		)
		if err := s.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
			return
		}
		errs <- nil
	}()

	select {
	case err := <-errs:
		if err != nil {
			return fmt.Errorf("controlplane: serve: %w", err)
		}
		return nil

	case <-ctx.Done():
		s.log.Info("shutting down", "timeout", s.cfg.Server.ShutdownTimeout)

		// Detached from ctx, which is already cancelled.
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.Server.ShutdownTimeout)
		defer cancel()

		if err := s.http.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("controlplane: graceful shutdown: %w", err)
		}
		return <-errs
	}
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// Operational responses are point-in-time. Caching a readiness result is
	// how a load balancer ends up routing to a dead instance.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)

	// The response is already committed by this point, so a failed encode can
	// only be logged, not corrected. Encoding a map of strings does not fail in
	// practice; if it ever does, the truncated body plus the access log line is
	// the signal.
	_ = json.NewEncoder(w).Encode(body)
}
