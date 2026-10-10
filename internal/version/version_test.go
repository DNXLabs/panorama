// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package version

import "testing"

func TestSourceLinksToTheCommitWhenThereIsOne(t *testing.T) {
	const repo = "https://github.com/example/fork"
	for _, c := range []struct{ commit, want string }{
		{"9a75838", repo + "/tree/9a75838"},
		{"9a758380c1d2e3f4a5b6c7d8e9f0a1b2c3d4e5f6", repo + "/tree/9a758380c1d2e3f4a5b6c7d8e9f0a1b2c3d4e5f6"},
		// Not a commit, so a /tree/ link would be a 404.
		{"unknown", repo},
		{"9a75838-dirty", repo},
		{"", repo},
	} {
		if got := sourceAt(repo, c.commit); got != c.want {
			t.Errorf("sourceAt(%q) = %q, want %q", c.commit, got, c.want)
		}
	}
}

func TestGetFillsTheSource(t *testing.T) {
	if Get().Source == "" {
		t.Error("Get().Source is empty; the console's source link would point nowhere")
	}
}
