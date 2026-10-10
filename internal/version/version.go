// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

// Package version carries build metadata stamped in at link time.
package version

import (
	"regexp"
	"runtime/debug"
)

// These are set with -ldflags at build time. See the Makefile.
var (
	// Version is the release version, e.g. "v1.4.2", or "dev" for a local build.
	Version = "dev"
	// Commit is the git SHA the binary was built from.
	Commit = "unknown"
	// Date is the RFC 3339 build timestamp.
	Date = "unknown"
	// SourceURL is the repository this binary's source is published in. The
	// console links to it at the running commit, which is how anybody using a
	// deployment over the network is offered its source, as AGPL-3.0 section
	// 13 asks. A fork that runs modified code for others sets it to its own
	// repository: -X github.com/dnxlabs/panorama/internal/version.SourceURL=...
	SourceURL = "https://github.com/dnxlabs/panorama"
)

// Info describes the running binary.
type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
	Go      string `json:"go"`
	// Source is where this build's source can be read: the repository at the
	// commit when the commit is known, the repository otherwise.
	Source string `json:"source"`
}

// Get returns the build metadata for this binary. When the binary was built
// without ldflags - `go run`, for example - the commit is recovered from the
// embedded VCS stamp so local builds still identify themselves.
func Get() Info {
	info := Info{Version: Version, Commit: Commit, Date: Date, Go: "unknown"}

	bi, ok := debug.ReadBuildInfo()
	if !ok {
		info.Source = sourceAt(SourceURL, info.Commit)
		return info
	}
	info.Go = bi.GoVersion

	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			if info.Commit == "unknown" {
				info.Commit = s.Value
			}
		case "vcs.time":
			if info.Date == "unknown" {
				info.Date = s.Value
			}
		}
	}
	info.Source = sourceAt(SourceURL, info.Commit)
	return info
}

// commitID is what a git commit looks like, abbreviated or not. Anything else
// ("unknown", a dirty-tree suffix) would make a link to a tree that does not
// exist, so it links to the repository instead.
var commitID = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

func sourceAt(repo, commit string) string {
	if commitID.MatchString(commit) {
		return repo + "/tree/" + commit
	}
	return repo
}

// String renders the build metadata for a startup log line.
func (i Info) String() string {
	return i.Version + " (" + i.Commit + ", built " + i.Date + ", " + i.Go + ")"
}
