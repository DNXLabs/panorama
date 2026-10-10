// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

// Command generate writes the configuration reference data the docs render.
//
//	go run ./internal/config/configdoc/generate -o docs/src/data/config-keys.json
//
// The committed file is what `make docs` reads, so the documentation build
// needs no Go toolchain. TestTheConfigurationReferenceIsCurrent asserts the two
// agree, which is what stops the committed copy from going stale: change a
// default, and the test fails at the desk of whoever changed it.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/dnxlabs/panorama/internal/config"
	"github.com/dnxlabs/panorama/internal/config/configdoc"
)

func main() {
	out := flag.String("o", "", "file to write (default stdout)")
	dir := flag.String("dir", "internal/config", "the config package to read")
	flag.Parse()

	keys, err := configdoc.Collect(*dir, config.Default())
	if err != nil {
		fmt.Fprintf(os.Stderr, "configdoc: %v\n", err)
		os.Exit(1)
	}

	body, err := configdoc.Marshal(keys)
	if err != nil {
		fmt.Fprintf(os.Stderr, "configdoc: %v\n", err)
		os.Exit(1)
	}

	if *out == "" {
		os.Stdout.Write(body)
		return
	}
	if err := os.WriteFile(*out, body, 0o644); err != nil { //nolint:gosec // a committed, public reference file, readable like every other tracked file
		fmt.Fprintf(os.Stderr, "configdoc: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "configdoc: wrote %d keys to %s\n", len(keys), *out)
}
