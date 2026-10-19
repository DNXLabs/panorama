// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

// Package cloudformation holds no code: the template is the artifact, and this
// package is where its renderer is tested.
package cloudformation

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestTheReleaseTemplateMeansTheSame renders the template the way release.yml
// does and parses both. release-template.sh drops comments to stay under the
// inline deploy limit, and a line dropped from inside a block scalar, or one
// kept that ended a mapping early, would still be valid YAML that deploys
// something else. So the two documents are compared node by node, and the only
// difference allowed is the release's own tag.
func TestTheReleaseTemplateMeansTheSame(t *testing.T) {
	source, err := os.ReadFile("panorama.yaml")
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := exec.Command("bash", "release-template.sh", "v9.8.7-test").Output()
	if err != nil {
		t.Fatalf("release-template.sh: %v", err)
	}

	var want, got yaml.Node
	if err := yaml.Unmarshal(source, &want); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(rendered, &got); err != nil {
		t.Fatalf("the rendered template is not YAML: %v", err)
	}

	var differences []string
	compare(&want, &got, "", &differences)
	wantDifference := "Mappings.Release.Image.Tag: \"stable\" became \"v9.8.7-test\""
	if len(differences) != 1 || differences[0] != wantDifference {
		t.Fatalf("rendering changed %d values, want only %s:\n%s",
			len(differences), wantDifference, strings.Join(differences, "\n"))
	}

	if strings.Count(string(rendered), "\n#") > 1 {
		t.Error("the rendered template still carries comments beyond its SPDX lines")
	}
	if len(rendered) > 51200 {
		t.Errorf("the rendered template is %d bytes; aws cloudformation deploy refuses more than 51,200 inline", len(rendered))
	}
}

func compare(want, got *yaml.Node, path string, differences *[]string) {
	if want.Kind != got.Kind || want.Tag != got.Tag || len(want.Content) != len(got.Content) {
		*differences = append(*differences, fmt.Sprintf("%s: shape changed", path))
		return
	}
	if want.Value != got.Value {
		*differences = append(*differences, fmt.Sprintf("%s: %q became %q", path, want.Value, got.Value))
	}
	for i := range want.Content {
		at := path
		switch {
		case want.Kind == yaml.MappingNode && i%2 == 1:
			at = strings.TrimPrefix(path+"."+want.Content[i-1].Value, ".")
		case want.Kind == yaml.SequenceNode:
			at = fmt.Sprintf("%s[%d]", path, i)
		}
		compare(want.Content[i], got.Content[i], at, differences)
	}
}
