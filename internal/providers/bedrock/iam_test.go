// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package bedrock

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestTaskRolePolicyMatchesTheTemplate holds the one policy value this package
// renders to the same statements the CloudFormation task role grants.
//
// The template is what a deployment actually runs with, and this value is what
// the console and the CLI tell an operator to paste. Two copies kept in step by
// hand drift, and the drift is found by somebody's first request failing
// AccessDenied - so the template is parsed rather than trusted.
//
// The execute-command statement is the template's own business: it serves the
// ECS agent, not the gateway, and an operator running Panorama anywhere else
// has no use for it.
func TestTaskRolePolicyMatchesTheTemplate(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy", "cloudformation", "panorama.yaml"))
	if err != nil {
		t.Fatalf("reading the template: %v", err)
	}

	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		t.Fatalf("parsing the template: %v", err)
	}

	policies := walk(t, &root, "Resources", "TaskRole", "Properties", "Policies")
	var document *yaml.Node
	for _, p := range policies.Content {
		if name := child(p, "PolicyName"); name != nil && name.Value == "bedrock" {
			document = child(p, "PolicyDocument")
		}
	}
	if document == nil {
		t.Fatal("the task role has no policy named bedrock")
	}

	statements := child(document, "Statement")
	if statements == nil {
		t.Fatal("the bedrock policy has no statements")
	}

	var got []PolicyStatement
	for _, s := range statements.Content {
		st := PolicyStatement{
			Sid:      scalar(child(s, "Sid")),
			Effect:   scalar(child(s, "Effect")),
			Action:   list(child(s, "Action")),
			Resource: list(child(s, "Resource")),
		}
		if st.Sid == "ExecuteCommandChannel" {
			continue
		}
		got = append(got, st)
	}

	want := TaskRolePolicy.Statement
	if len(got) != len(want) {
		t.Fatalf("the template grants %d statements and TaskRolePolicy %d:\n template: %v\n value:    %v",
			len(got), len(want), sids(got), sids(want))
	}
	for i := range want {
		if !equalStatement(got[i], want[i]) {
			g, _ := json.Marshal(got[i])
			w, _ := json.Marshal(want[i])
			t.Errorf("statement %d differs:\n template: %s\n value:    %s", i, g, w)
		}
	}
}

// Both endpoints, in every region, with no model condition: the premise that
// changing a provider's region in the console never needs an IAM change.
func TestTaskRolePolicyCoversBothEndpointsEverywhere(t *testing.T) {
	rendered := TaskRolePolicyJSON()
	for _, want := range []string{
		`"bedrock:InvokeModelWithResponseStream"`,
		`"bedrock-mantle:CreateInference"`,
		`"bedrock-mantle:ListModels"`,
		`"arn:aws:bedrock-mantle:*:*:project/*"`,
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the rendered policy does not contain %s", want)
		}
	}
	for _, refused := range []string{"Condition", "CallWithBearerToken", "us-east-1"} {
		if strings.Contains(rendered, refused) {
			t.Errorf("the rendered policy contains %q; it should grant both endpoints in every region, unconditionally", refused)
		}
	}
}

func walk(t *testing.T, n *yaml.Node, path ...string) *yaml.Node {
	t.Helper()
	cur := n
	if cur.Kind == yaml.DocumentNode && len(cur.Content) > 0 {
		cur = cur.Content[0]
	}
	for _, key := range path {
		next := child(cur, key)
		if next == nil {
			t.Fatalf("the template has no %s", strings.Join(path, "."))
		}
		cur = next
	}
	return cur
}

func child(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

func scalar(n *yaml.Node) string {
	if n == nil {
		return ""
	}
	return n.Value
}

// list reads a value IAM accepts as either one string or a list of them.
func list(n *yaml.Node) []string {
	if n == nil {
		return nil
	}
	if n.Kind == yaml.ScalarNode {
		return []string{n.Value}
	}
	out := make([]string, 0, len(n.Content))
	for _, c := range n.Content {
		out = append(out, c.Value)
	}
	return out
}

func equalStatement(a, b PolicyStatement) bool {
	return a.Sid == b.Sid && a.Effect == b.Effect &&
		slices.Equal(a.Action, b.Action) && slices.Equal(a.Resource, b.Resource)
}

func sids(statements []PolicyStatement) []string {
	out := make([]string, 0, len(statements))
	for _, s := range statements {
		out = append(out, s.Sid)
	}
	return out
}
