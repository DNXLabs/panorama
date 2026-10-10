// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func timeNow() time.Time { return time.Now().UTC() }

// TestConsoleIsEmbedded proves the console actually shipped inside the binary.
//
// It is not a formality: the console is built by a separate toolchain into a
// directory that is not committed, so a binary built without running `make ui`
// is a blank page for whoever deployed it - and go:embed fails silently in the
// sense that nothing else notices.
func TestConsoleIsEmbedded(t *testing.T) {
	files, err := consoleFiles()
	if err != nil {
		t.Fatalf("read embedded console: %v", err)
	}

	var (
		index  bool
		script bool
		style  bool
		font   bool
	)
	for _, f := range files {
		switch {
		case f == "index.html":
			index = true
		case strings.HasPrefix(f, "assets/") && strings.HasSuffix(f, ".js"):
			script = true
		case strings.HasPrefix(f, "assets/") && strings.HasSuffix(f, ".css"):
			style = true
		case strings.HasSuffix(f, ".woff2"):
			font = true
		}
	}

	for name, found := range map[string]bool{
		"the entry document": index,
		"a script bundle":    script,
		"a stylesheet":       style,
		// The fonts are bundled rather than fetched from a CDN. An air-gapped
		// deployment that called Google Fonts on every page load would
		// contradict the claim the product is sold on.
		"a self-hosted font": font,
	} {
		if !found {
			t.Errorf("%s is not embedded in the binary; run `make ui`", name)
		}
	}
}

func TestConsolePageIsServed(t *testing.T) {
	f := newConsole(t)

	rec := httptest.NewRecorder()
	f.server.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Panorama") {
		t.Error("the console page does not look like the console")
	}
}

// TestClientSideRoutesServeTheConsole. Every screen has a real URL, so a
// bookmark, a reload, and a link from a colleague all land somewhere.
func TestClientSideRoutesServeTheConsole(t *testing.T) {
	f := newConsole(t)

	for _, path := range []string{"/work", "/requests", "/policies", "/me", "/setup", "/welcome"} {
		rec := httptest.NewRecorder()
		f.server.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

		if rec.Code != http.StatusOK {
			t.Errorf("%s = %d, want the console", path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
			t.Errorf("%s served %q, want html", path, ct)
		}
	}
}

// TestUnknownAPIPathsAnswerAsAPIPaths. Go's mux routes `GET /` as a catch-all,
// which would otherwise answer an unknown API path with a page of markup - and
// a client that asked for JSON and got HTML reports the wrong problem.
func TestUnknownAPIPathsAnswerAsAPIPaths(t *testing.T) {
	f := newConsole(t)

	for _, path := range []string{"/api/v1/nope", "/api/nope", "/v1/nope", "/auth/nope"} {
		rec := httptest.NewRecorder()
		f.server.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

		if rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "json") {
			t.Errorf("%s answered %q, want json", path, ct)
		}
	}
}

func TestConsoleAssetsAreServed(t *testing.T) {
	f := newConsole(t)

	files, err := consoleFiles()
	if err != nil {
		t.Fatalf("read embedded console: %v", err)
	}

	var served int
	for _, file := range files {
		if !strings.HasPrefix(file, "assets/") {
			continue
		}
		rec := httptest.NewRecorder()
		f.server.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/"+file, nil))

		if rec.Code != http.StatusOK {
			t.Errorf("/%s = %d, want 200", file, rec.Code)
			continue
		}
		// Content-hashed, so it may be cached forever: the name changes
		// whenever the contents do.
		if cache := rec.Header().Get("Cache-Control"); !strings.Contains(cache, "immutable") {
			t.Errorf("/%s Cache-Control = %q, want it cacheable", file, cache)
		}
		served++
	}
	if served == 0 {
		t.Fatal("no built assets were served")
	}
}

// TestEveryAssetTheEntryDocumentNamesIsServed.
//
// This exists because of a bug the other asset tests could not see. They fetch
// each embedded file by the path it sits at, which passes whatever the built
// markup happens to reference - and a misconfigured base path had index.html
// asking for /assets/assets/index-hash.js while the file sat at
// /assets/index-hash.js. Every Go test was green and the console was a blank
// page with three 404s in it. Following the document's own references is the
// only version of this check that means anything.
func TestEveryAssetTheEntryDocumentNamesIsServed(t *testing.T) {
	f := newConsole(t)

	rec := httptest.NewRecorder()
	f.server.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	document := rec.Body.String()

	// Deliberately a crude scan rather than an HTML parser: it has to see every
	// URL the browser will fetch, and being fooled by clever markup is not a
	// risk in a document this build produces.
	var referenced []string
	for _, attribute := range []string{`src="`, `href="`} {
		rest := document
		for {
			start := strings.Index(rest, attribute)
			if start < 0 {
				break
			}
			rest = rest[start+len(attribute):]
			end := strings.Index(rest, `"`)
			if end < 0 {
				break
			}
			if url := rest[:end]; strings.HasPrefix(url, "/") {
				referenced = append(referenced, url)
			}
			rest = rest[end:]
		}
	}

	if len(referenced) == 0 {
		t.Fatal("the entry document references no assets at all")
	}

	for _, url := range referenced {
		asset := httptest.NewRecorder()
		f.server.mux.ServeHTTP(asset, httptest.NewRequest(http.MethodGet, url, nil))
		if asset.Code != http.StatusOK {
			t.Errorf("index.html references %s, which answers %d", url, asset.Code)
		}
	}
}

// TestTheEntryDocumentIsNotCached. It names the hashed asset filenames, so a
// cached one after an upgrade is a console fetching files that no longer exist.
func TestTheEntryDocumentIsNotCached(t *testing.T) {
	f := newConsole(t)

	rec := httptest.NewRecorder()
	f.server.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if cache := rec.Header().Get("Cache-Control"); !strings.Contains(cache, "no-cache") {
		t.Errorf("Cache-Control = %q, want no-cache on the entry document", cache)
	}
}

// TestConsoleShipsAStrictCSP matters more than usual here: the page holds a
// session, and a session in a browser is only as safe as the page holding it.
func TestConsoleShipsAStrictCSP(t *testing.T) {
	f := newConsole(t)

	rec := httptest.NewRecorder()
	f.server.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	csp := rec.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("the console is served without a content security policy")
	}
	for _, directive := range []string{
		"default-src 'none'",
		"script-src 'self'",
		"connect-src 'self'",
		"font-src 'self'",
		"frame-ancestors 'none'",
	} {
		if !strings.Contains(csp, directive) {
			t.Errorf("CSP is missing %q: %s", directive, csp)
		}
	}
	// An inline script would defeat the point, and would also mean the page had
	// started carrying data in its markup.
	if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
		t.Errorf("CSP permits unsafe inline code: %s", csp)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
}

// TestConsolePageCarriesNoOrganizationData is the reason the static shell is
// allowed to be unauthenticated at all.
func TestConsolePageCarriesNoOrganizationData(t *testing.T) {
	f := newConsole(t)
	f.record(t, f.admin.ID, "secret-request", "us.anthropic.claude-sonnet-5", money(1), timeNow())

	files, err := consoleFiles()
	if err != nil {
		t.Fatalf("read embedded console: %v", err)
	}

	paths := []string{"/"}
	for _, file := range files {
		if strings.HasPrefix(file, "assets/") && !strings.HasSuffix(file, ".woff2") {
			paths = append(paths, "/"+file)
		}
	}

	for _, path := range paths {
		rec := httptest.NewRecorder()
		f.server.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

		body := rec.Body.String()
		for _, secret := range []string{"secret-request", "admin@example.com", f.adminToken} {
			if strings.Contains(body, secret) {
				t.Errorf("%s leaks %q to an unauthenticated visitor", path, secret)
			}
		}
	}
}
