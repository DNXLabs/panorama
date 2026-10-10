// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"io/fs"
	"net/http"
	"strings"

	"github.com/dnxlabs/panorama/internal/integrations"
	"github.com/dnxlabs/panorama/internal/policy"
	"github.com/dnxlabs/panorama/internal/pricing"
	"github.com/dnxlabs/panorama/internal/providers"
	"github.com/dnxlabs/panorama/internal/secrets"
	"github.com/dnxlabs/panorama/internal/websearch"
	"github.com/dnxlabs/panorama/ui"
)

// consoleCSP is the content security policy the console is served under.
//
// It is this strict because the page holds a session, and a session in a
// browser is only as safe as the page holding it. Nothing loads from anywhere
// but this origin, no inline script runs, and the page cannot be framed.
//
// It also enforces the product's own claim: a console that cannot reach a third
// party cannot leak to one, which is a large part of what an air-gapped
// deployment is buying. The fonts are bundled and served from here for exactly
// that reason - the hi-fi mock loads them from a CDN, and shipping that would
// have made every page load call out.
const consoleCSP = "default-src 'none'; " +
	"script-src 'self'; " +
	"style-src 'self'; " +
	"connect-src 'self'; " +
	"img-src 'self' data:; " +
	"font-src 'self'; " +
	"base-uri 'none'; " +
	"form-action 'none'; " +
	"frame-ancestors 'none'"

// ConsoleOptions is everything the console API needs beyond the store.
//
// Passed as a struct because it has grown past the point where positional
// arguments say what they are, and because a deployment can be missing most of
// it: no provider configured, no integrations connected, no SSO. Every field
// here is optional except the first two, and every screen that depends on an
// absent one says so rather than showing a zero.
type ConsoleOptions struct {
	Prices *pricing.Table
	Policy *policy.Engine

	// Integrations is the connected systems of record. Nil is a supported
	// deployment.
	Integrations *integrations.Resolver
	Secrets      *secrets.Box

	// Providers builds the configured model providers, for the screens that
	// configure, check and discover them. A deployment with no provider row yet
	// still has a registry: it is what knows which kinds could be configured.
	Providers *providers.Registry
	// PriceService refreshes a provider's rates when its region changes. Nil
	// leaves pricing to its own schedule.
	PriceService *pricing.Service
	// WebSearch holds the web search backend. Nil is a deployment with none;
	// the settings screen still saves, and nothing serves until a restart.
	WebSearch *websearch.Service
	// WebSearchKind describes the backend, so the console can check an
	// address and name the permission without importing it.
	WebSearchKind websearch.Descriptor
	// WebSearchDeployed is the gateway the deployment created for itself, if
	// its configuration names one. The screen then shows it and does not
	// offer to change it.
	WebSearchDeployed websearch.Deployment
}

// MountConsole registers the console API and the embedded page.
func (s *Server) MountConsole(opts ConsoleOptions) {
	api := &API{
		db:            s.db,
		prices:        opts.Prices,
		policy:        opts.Policy,
		integrations:  opts.Integrations,
		secrets:       opts.Secrets,
		providers:     opts.Providers,
		priceService:  opts.PriceService,
		webSearch:     opts.WebSearch,
		webSearchKind: opts.WebSearchKind,
		webSearchDep:  opts.WebSearchDeployed,
		server:        s,
		log:           s.log,
	}
	s.mountAPI(api)
	s.mountAdmin(api)
	s.mountExports(api)
	s.mountAssets()
}

// mountAssets serves the console.
//
// These are the only routes besides the probes and the sign-in surface that are
// not authenticated, and the reason is narrow: they are a static shell -
// markup, stylesheet, script, and two fonts - identical for every deployment
// and every visitor, containing no organizational data. Everything the console
// displays comes from the authenticated API, so an unauthenticated visitor gets
// a sign-in screen. Anything added here must keep that true.
func (s *Server) mountAssets() {
	files := ui.Assets()

	index, err := ui.Index()
	if err != nil {
		// A binary built without the console is a real state - someone ran
		// `go build` without `make ui` - and it should fail loudly at startup
		// rather than serve an empty page.
		s.log.Error("the console was not built into this binary", "error", err,
			"hint", "run `make ui` before building, or use `make build` which does")
		return
	}

	// Hashed filenames, so they may be cached hard. The entry document may not:
	// it names the hashes, and a cached one after an upgrade is a console
	// fetching assets that no longer exist.
	s.mux.Handle("GET /assets/", immutableAsset(http.FileServerFS(files)))
	s.mux.Handle("GET /", secureAsset(spa(index)))
}

// spa serves the console's entry document for every path the console owns.
//
// Go's mux routes `GET /` as a catch-all, which would otherwise answer an
// unknown API path with a page of HTML - and a client that asked for JSON and
// got markup reports the wrong problem. So the prefixes that belong to the API,
// the proxy, and the probes are refused here explicitly, in the shape their
// callers expect.
func spa(index []byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, prefix := range []string{"/api/", "/v1/", "/auth/", "/healthz", "/readyz"} {
			if strings.HasPrefix(r.URL.Path, prefix) {
				writeJSON(w, http.StatusNotFound, map[string]any{
					"error": "No such endpoint.",
					"path":  r.URL.Path,
				})
				return
			}
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(index)
	})
}

// secureAsset adds the console's security headers.
func secureAsset(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", consoleCSP)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// immutableAsset serves a content-hashed file, which may be cached forever
// because its name changes whenever its contents do.
func immutableAsset(next http.Handler) http.Handler {
	return secureAsset(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		next.ServeHTTP(w, r)
	}))
}

// consoleFiles lists the embedded assets, for the test that proves the console
// actually shipped inside the binary.
func consoleFiles() ([]string, error) {
	var out []string
	err := fs.WalkDir(ui.Assets(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && !strings.HasPrefix(path, ".") {
			out = append(out, path)
		}
		return nil
	})
	return out, err
}
