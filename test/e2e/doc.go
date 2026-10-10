// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

// Package e2e drives a real Panorama deployment end to end.
//
// Every file in it is behind the `e2e` build tag, because the tests build the
// binary, bind ports, and - where the tool is installed - run a real coding
// agent. They are not part of `make test`:
//
//	make e2e        # this package, against internal/fakebedrock
//	make e2e-live   # the narrow set that needs real AWS credentials
//
// This file carries no build tag on purpose. A directory whose every file is
// excluded by a tag is not a package as far as `go build ./...` is concerned,
// and the error it reports - "build constraints exclude all Go files" - would
// break the ordinary build for everyone who never runs this suite.
package e2e
