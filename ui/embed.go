// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

// Package ui holds the console, embedded into the binary.
//
// The source is in app/ - React and TypeScript, built by Vite - and the built
// output in dist/. Only dist/ is embedded, and it is not committed: `make ui`
// produces it in a container, and every Go command in this repository depends
// on that target, so `make build`, `make test`, and `make check` all build the
// console first.
//
// That is a real cost: `go build ./...` on its own does not work from a fresh
// clone, because embed refuses to compile against an empty directory. It is
// preferred over committing generated assets, which would put a minified bundle
// in every diff that touches a screen.
package ui

import (
	"embed"
	"io/fs"
)

// The `all:` prefix is load-bearing: without it, embed skips files whose names
// begin with a dot or an underscore, and a bundler is entitled to emit those.
//
//go:embed all:dist
var assets embed.FS

// Assets returns the built console, rooted so that index.html is at the top.
func Assets() fs.FS {
	sub, err := fs.Sub(assets, "dist")
	if err != nil {
		// The embed directive above is a compile-time constant, so this cannot
		// fail at runtime; panicking beats returning an error every caller
		// would have to ignore.
		panic("ui: console assets are missing: " + err.Error())
	}
	return sub
}

// Index returns the console's entry document, which is served for every
// client-side route rather than only for /.
func Index() ([]byte, error) {
	return assets.ReadFile("dist/index.html")
}
